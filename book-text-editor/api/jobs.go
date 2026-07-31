package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type jobRunner struct {
	server *Server
	queue  chan jobTask

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
	once   sync.Once
	wg     sync.WaitGroup
}

func newJobRunner(server *Server, queueSize int) *jobRunner {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &jobRunner{
		server: server,
		queue:  make(chan jobTask, queueSize),
		ctx:    ctx,
		cancel: cancel,
	}
	runner.wg.Add(1)
	go runner.loop()
	return runner
}

func (r *jobRunner) context() context.Context { return r.ctx }

func (r *jobRunner) enqueue(task jobTask) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	select {
	case r.queue <- task:
		return true
	default:
		return false
	}
}

func (r *jobRunner) close() {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.cancel()
		r.mu.Unlock()
		r.wg.Wait()
	})
}

func (r *jobRunner) loop() {
	defer r.wg.Done()
	for {
		select {
		case <-r.ctx.Done():
			return
		case task := <-r.queue:
			r.process(task)
		}
	}
}

func (r *jobRunner) process(task jobTask) {
	if err := r.server.store.startTask(
		r.ctx,
		task,
		r.server.now().UTC(),
	); err != nil {
		r.server.logger.Error(
			"start generation task",
			"job_id", task.JobID,
			"error", err,
		)
		return
	}

	for _, fragmentID := range task.FragmentIDs {
		if r.ctx.Err() != nil {
			return
		}
		if err := r.processFragment(task, fragmentID); err != nil {
			r.server.logger.Error(
				"process generation fragment",
				"job_id", task.JobID,
				"fragment_id", fragmentID,
				"error", err,
			)
		}
	}

	if err := r.server.store.finishTask(
		r.ctx,
		task.JobID,
		r.server.now().UTC(),
	); err != nil {
		r.server.logger.Error(
			"finish generation task",
			"job_id", task.JobID,
			"error", err,
		)
	}
}

func (r *jobRunner) processFragment(task jobTask, fragmentID string) error {
	retriesRemaining := task.Settings.AutomaticWarningRetries
	usedSeeds := make(map[uint32]struct{}, retriesRemaining+1)

	for {
		warningCode, seed, err := r.processFragmentAttempt(
			task,
			fragmentID,
			usedSeeds,
		)
		if err != nil {
			return err
		}
		if !automaticRetryWarningCode(warningCode) || retriesRemaining == 0 {
			return nil
		}
		if err := r.server.store.prepareAutomaticWarningRetry(
			r.ctx,
			fragmentID,
			r.server.now().UTC(),
		); err != nil {
			return fmt.Errorf("prepare automatic warning retry: %w", err)
		}
		retriesRemaining--
		usedSeeds[seed] = struct{}{}
	}
}

func (r *jobRunner) processFragmentAttempt(
	task jobTask,
	fragmentID string,
	usedSeeds map[uint32]struct{},
) (string, uint32, error) {
	started, err := r.server.store.startFragment(
		r.ctx,
		fragmentID,
		r.server.now().UTC(),
	)
	if err != nil {
		return "", 0, fmt.Errorf("mark fragment as generating: %w", err)
	}
	item, err := r.server.store.workItem(r.ctx, fragmentID)
	if err != nil {
		r.failFragment(fragmentID, "fragment dependencies are unavailable", err)
		return "", 0, err
	}
	item.Resource = started

	seed, err := randomWorkerSeed(usedSeeds)
	if err != nil {
		r.failFragment(fragmentID, "generation seed is unavailable", err)
		return "", 0, fmt.Errorf("generate worker seed: %w", err)
	}

	ttsContext, cancelTTS := r.server.workerContext()
	ttsResult, err := r.server.tts.Generate(ttsContext, TTSRequest{
		RequestID:            r.server.newID(),
		JobID:                task.JobID,
		FragmentID:           fragmentID,
		Text:                 item.Resource.Text,
		ReferenceAudio:       item.Voice.Audio,
		ReferenceContentType: item.Voice.Resource.ContentType,
		ReferenceText:        item.Voice.ReferenceText,
		NumSteps:             task.Settings.OmniVoice.NumSteps,
		GuidanceScale:        task.Settings.OmniVoice.GuidanceScale,
		Speed:                task.Settings.OmniVoice.Speed,
		NormalizeText:        task.Settings.OmniVoice.NormalizeText,
		Denoise:              task.Settings.OmniVoice.Denoise,
		TShift:               task.Settings.OmniVoice.TShift,
		LayerPenaltyFactor:   task.Settings.OmniVoice.LayerPenaltyFactor,
		PositionTemperature:  task.Settings.OmniVoice.PositionTemperature,
		ClassTemperature:     task.Settings.OmniVoice.ClassTemperature,
		PreprocessPrompt:     task.Settings.OmniVoice.PreprocessPrompt,
		PostprocessOutput:    task.Settings.OmniVoice.PostprocessOutput,
		AudioChunkDuration:   task.Settings.OmniVoice.AudioChunkDuration,
		AudioChunkThreshold:  task.Settings.OmniVoice.AudioChunkThreshold,
		PadDuration:          task.Settings.OmniVoice.PadDuration,
		FadeDuration:         task.Settings.OmniVoice.FadeDuration,
		Seed:                 &seed,
		SettingsResolved:     true,
	})
	cancelTTS()
	if err != nil {
		if errors.Is(err, context.Canceled) && r.ctx.Err() != nil {
			r.failFragment(fragmentID, "generation interrupted", err)
			return "", seed, err
		}
		r.failFragment(fragmentID, "OmniVoice generation failed", err)
		return "", seed, fmt.Errorf("call OmniVoice: %w", err)
	}
	if len(ttsResult.AudioPCM) == 0 {
		err = errors.New("OmniVoice returned empty PCM")
		r.failFragment(fragmentID, "OmniVoice returned invalid audio", err)
		return "", seed, err
	}

	sttContext, cancelSTT := r.server.workerContext()
	sttResult, err := r.server.stt.Transcribe(sttContext, STTRequest{
		RequestID:        r.server.newID(),
		JobID:            task.JobID,
		FragmentID:       fragmentID,
		AudioPCM:         ttsResult.AudioPCM,
		SampleRate:       ttsResult.SampleRate,
		Channels:         ttsResult.Channels,
		SampleWidth:      ttsResult.SampleWidth,
		Language:         "ru",
		ExpectedText:     item.Resource.Text,
		BeamSize:         task.Settings.Whisper.BeamSize,
		Patience:         task.Settings.Whisper.Patience,
		Temperature:      task.Settings.Whisper.Temperature,
		VADFilter:        task.Settings.Whisper.VADFilter,
		WordTimestamps:   task.Settings.Whisper.WordTimestamps,
		SettingsResolved: true,
	})
	cancelSTT()
	if err != nil {
		if errors.Is(err, context.Canceled) && r.ctx.Err() != nil {
			r.failFragment(fragmentID, "validation interrupted", err)
			return "", seed, err
		}
		r.failFragment(fragmentID, "speech validation failed", err)
		return "", seed, fmt.Errorf("call STT: %w", err)
	}

	warningCode := ""
	if !transcriptMatches(item.Resource.Text, sttResult.Text) {
		warningCode = "transcript_mismatch"
	} else if len(ttsResult.Warnings) > 0 {
		warningCode = "audio_warning"
	}

	err = r.server.store.completeFragment(
		r.ctx,
		fragmentID,
		fragmentResult{
			AudioPCM:    ttsResult.AudioPCM,
			SampleRate:  ttsResult.SampleRate,
			Channels:    ttsResult.Channels,
			SampleWidth: ttsResult.SampleWidth,
			DurationMS:  ttsResult.DurationMS,
			STTText:     sttResult.Text,
			STTLanguage: sttResult.Language,
			WarningCode: warningCode,
			WorkerNotes: append([]string(nil), ttsResult.Warnings...),
		},
		r.server.now().UTC(),
	)
	if err != nil {
		return "", seed, fmt.Errorf("save fragment result: %w", err)
	}
	return warningCode, seed, nil
}

func (r *jobRunner) failFragment(
	fragmentID, publicMessage string,
	cause error,
) {
	r.server.logger.Error(
		"worker task failed",
		"fragment_id", fragmentID,
		"error", cause,
	)
	if err := r.server.store.failFragment(
		context.Background(),
		fragmentID,
		publicMessage,
		r.server.now().UTC(),
	); err != nil {
		r.server.logger.Error(
			"persist fragment failure",
			"fragment_id", fragmentID,
			"error", err,
		)
	}
}

// transcriptMatches intentionally tolerates small, typical STT deviations but
// still rejects missing phrases, reordered content and material substitutions.
// Punctuation, case, accents and е/ё differences are ignored before scoring.
func transcriptMatches(expected, actual string) bool {
	expectedNormalized := normalizeValidationText(expected)
	actualNormalized := normalizeValidationText(actual)
	if expectedNormalized == actualNormalized {
		return expectedNormalized != ""
	}
	if expectedNormalized == "" || actualNormalized == "" {
		return false
	}

	expectedTokens := canonicalValidationTokens(expectedNormalized)
	actualTokens := canonicalValidationTokens(actualNormalized)
	if len(expectedTokens) <= 2 || len(actualTokens) <= 2 {
		return false
	}

	maxTokenDistance := max(1, int(math.Ceil(float64(len(expectedTokens))*0.08)))
	if len(expectedTokens) <= 7 {
		maxTokenDistance = 1
	}
	if absInt(len(expectedTokens)-len(actualTokens)) > maxTokenDistance {
		return false
	}
	if tokenEditDistance(expectedTokens, actualTokens) > maxTokenDistance {
		return false
	}

	expectedRunes := []rune(strings.Join(expectedTokens, ""))
	actualRunes := []rune(strings.Join(actualTokens, ""))
	longer := max(len(expectedRunes), len(actualRunes))
	if longer == 0 {
		return false
	}
	characterSimilarity := 1 - float64(
		runeEditDistance(expectedRunes, actualRunes),
	)/float64(longer)
	minimumSimilarity := 0.88
	if len(expectedTokens) <= 7 {
		minimumSimilarity = 0.92
	}
	return characterSimilarity >= minimumSimilarity
}

func normalizeValidationText(value string) string {
	value = cases.Fold().String(norm.NFKD.String(value))
	var builder strings.Builder
	builder.Grow(len(value))
	previousSpace := true

	for _, character := range value {
		if unicode.Is(unicode.Mn, character) {
			continue
		}
		if character == 'ё' {
			character = 'е'
		}
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			builder.WriteRune(character)
			previousSpace = false
			continue
		}
		if !previousSpace {
			builder.WriteByte(' ')
			previousSpace = true
		}
	}
	return strings.TrimSpace(builder.String())
}

var validationNumberWords = map[string]struct{}{
	"ноль": {}, "нуль": {}, "один": {}, "одна": {}, "одно": {},
	"два": {}, "две": {}, "три": {}, "четыре": {}, "пять": {},
	"шесть": {}, "семь": {}, "восемь": {}, "девять": {}, "десять": {},
	"одиннадцать": {}, "двенадцать": {}, "тринадцать": {},
	"четырнадцать": {}, "пятнадцать": {}, "шестнадцать": {},
	"семнадцать": {}, "восемнадцать": {}, "девятнадцать": {},
	"двадцать": {}, "тридцать": {}, "сорок": {}, "пятьдесят": {},
	"шестьдесят": {}, "семьдесят": {}, "восемьдесят": {}, "девяносто": {},
	"сто": {}, "двести": {}, "триста": {}, "четыреста": {},
	"пятьсот": {}, "шестьсот": {}, "семьсот": {}, "восемьсот": {},
	"девятьсот": {}, "тысяча": {}, "тысячи": {}, "тысяч": {},
	"миллион": {}, "миллиона": {}, "миллионов": {},
	"миллиард": {}, "миллиарда": {}, "миллиардов": {},
	"первый": {}, "первая": {}, "первое": {}, "первого": {},
	"второй": {}, "вторая": {}, "второе": {}, "второго": {},
	"третий": {}, "третья": {}, "третье": {}, "третьего": {},
}

func canonicalValidationTokens(normalized string) []string {
	raw := strings.Fields(normalized)
	result := make([]string, 0, len(raw))
	for _, token := range raw {
		canonical := token
		if isValidationNumberToken(token) {
			canonical = "<number>"
		}
		if canonical == "<number>" && len(result) > 0 &&
			result[len(result)-1] == canonical {
			continue
		}
		result = append(result, canonical)
	}
	return result
}

func isValidationNumberToken(token string) bool {
	if _, ok := validationNumberWords[token]; ok {
		return true
	}
	hasDigit := false
	for _, character := range token {
		if unicode.IsDigit(character) {
			hasDigit = true
			continue
		}
		return false
	}
	return hasDigit
}

func tokenEditDistance(left, right []string) int {
	previous := make([]int, len(right)+1)
	current := make([]int, len(right)+1)
	for index := range previous {
		previous[index] = index
	}
	for leftIndex, leftToken := range left {
		current[0] = leftIndex + 1
		for rightIndex, rightToken := range right {
			cost := 1
			if leftToken == rightToken {
				cost = 0
			}
			current[rightIndex+1] = min(
				previous[rightIndex+1]+1,
				current[rightIndex]+1,
				previous[rightIndex]+cost,
			)
		}
		previous, current = current, previous
	}
	return previous[len(right)]
}

func runeEditDistance(left, right []rune) int {
	previous := make([]int, len(right)+1)
	current := make([]int, len(right)+1)
	for index := range previous {
		previous[index] = index
	}
	for leftIndex, leftRune := range left {
		current[0] = leftIndex + 1
		for rightIndex, rightRune := range right {
			cost := 1
			if leftRune == rightRune {
				cost = 0
			}
			current[rightIndex+1] = min(
				previous[rightIndex+1]+1,
				current[rightIndex]+1,
				previous[rightIndex]+cost,
			)
		}
		previous, current = current, previous
	}
	return previous[len(right)]
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func logJobState(logger *slog.Logger, job JobResource) {
	logger.Debug(
		"job state",
		"job_id", job.ID,
		"status", job.Status,
		"pending", job.FragmentsPending,
		"ready", job.FragmentsReady,
		"warnings", job.FragmentsWarnings,
		"failed", job.FragmentsFailed,
		"updated_at", job.UpdatedAt.Format(time.RFC3339Nano),
	)
}
