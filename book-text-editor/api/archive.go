package api

import (
	"archive/zip"
	"bytes"
	"cmp"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tphakala/go-flac/pcm"
	"golang.org/x/text/unicode/norm"
)

const (
	fullArchiveManifestVersion       = "audiobook-chapter-flac-archive-v2"
	chapterArchiveManifestVersion    = "audiobook-single-chapter-flac-archive-v3"
	chapterFLACCompressionLevel      = 5
	maxChapterFilenameTitleRunes     = 120
	maxChapterAudioFilenameUTF8Bytes = 240
)

type archiveManifest struct {
	Version   string                          `json:"version"`
	Book      BookResource                    `json:"book"`
	Voice     VoiceResource                   `json:"voice"`
	Job       JobResource                     `json:"job"`
	Chapters  []chapterArchiveManifestChapter `json:"chapters"`
	Fragments []archiveManifestEntry          `json:"fragments"`
}

type archiveManifestEntry struct {
	ID            string         `json:"id"`
	ChapterNumber int            `json:"chapter_number"`
	Ordinal       int            `json:"ordinal"`
	Path          string         `json:"path"`
	StartSample   uint64         `json:"start_sample"`
	Samples       uint64         `json:"samples"`
	Text          string         `json:"text"`
	STTText       string         `json:"stt_text"`
	Status        FragmentStatus `json:"status"`
	Attempt       int            `json:"attempt"`
	DurationMS    int            `json:"duration_ms"`
}

type chapterArchiveManifest struct {
	Version   string                        `json:"version"`
	Scope     string                        `json:"scope"`
	Chapter   chapterArchiveManifestChapter `json:"chapter"`
	Book      BookResource                  `json:"book"`
	Voice     VoiceResource                 `json:"voice"`
	Job       JobResource                   `json:"job"`
	Fragments []archiveManifestEntry        `json:"fragments"`
}

type chapterArchiveManifestChapter struct {
	ChapterNumber  int    `json:"chapter_number"`
	Title          string `json:"title"`
	FragmentsCount int    `json:"fragments_count"`
	DurationMS     int64  `json:"duration_ms"`
	Path           string `json:"path"`
	AudioFormat    string `json:"audio_format"`
	SampleRate     int    `json:"sample_rate"`
	Channels       int    `json:"channels"`
	BitsPerSample  int    `json:"bits_per_sample"`
	TotalSamples   uint64 `json:"total_samples"`
	PCMBytes       int64  `json:"pcm_bytes"`
}

type encodedArchiveChapter struct {
	manifest  chapterArchiveManifestChapter
	audioFLAC []byte
	fragments []archiveManifestEntry
}

func buildAudioZIP(snapshot archiveSnapshot) ([]byte, error) {
	chapters, fragments, err := encodeArchiveChapters(snapshot.Fragments)
	if err != nil {
		return nil, err
	}
	manifestChapters := make(
		[]chapterArchiveManifestChapter,
		0,
		len(chapters),
	)
	for _, chapter := range chapters {
		manifestChapters = append(manifestChapters, chapter.manifest)
	}
	manifest, err := encodeArchiveManifest(archiveManifest{
		Version:   fullArchiveManifestVersion,
		Book:      snapshot.Book,
		Voice:     snapshot.Voice,
		Job:       snapshot.Job,
		Chapters:  manifestChapters,
		Fragments: fragments,
	})
	if err != nil {
		return nil, fmt.Errorf("encode archive manifest: %w", err)
	}
	return buildZIPArchive(snapshot, chapters, manifest)
}

func buildChapterAudioZIP(snapshot archiveSnapshot) ([]byte, error) {
	chapters, fragments, err := encodeArchiveChapters(snapshot.Fragments)
	if err != nil {
		return nil, err
	}
	if len(chapters) != 1 {
		return nil, errors.New(
			"chapter archive contains multiple chapters",
		)
	}
	manifest, err := encodeArchiveManifest(chapterArchiveManifest{
		Version:   chapterArchiveManifestVersion,
		Scope:     "chapter",
		Chapter:   chapters[0].manifest,
		Book:      snapshot.Book,
		Voice:     snapshot.Voice,
		Job:       snapshot.Job,
		Fragments: fragments,
	})
	if err != nil {
		return nil, fmt.Errorf("encode chapter archive manifest: %w", err)
	}
	return buildZIPArchive(snapshot, chapters, manifest)
}

func buildZIPArchive(
	snapshot archiveSnapshot,
	chapters []encodedArchiveChapter,
	manifest []byte,
) ([]byte, error) {
	var output bytes.Buffer
	zipWriter := zip.NewWriter(&output)
	for _, chapter := range chapters {
		header := &zip.FileHeader{
			Name:   chapter.manifest.Path,
			Method: zip.Store,
		}
		header.SetMode(0o644)
		header.SetModTime(snapshot.Job.UpdatedAt)
		entry, err := zipWriter.CreateHeader(header)
		if err != nil {
			_ = zipWriter.Close()
			return nil, fmt.Errorf(
				"create chapter %d ZIP entry: %w",
				chapter.manifest.ChapterNumber,
				err,
			)
		}
		if _, err := entry.Write(chapter.audioFLAC); err != nil {
			_ = zipWriter.Close()
			return nil, fmt.Errorf(
				"write chapter %d ZIP entry: %w",
				chapter.manifest.ChapterNumber,
				err,
			)
		}
	}

	header := &zip.FileHeader{Name: "manifest.json", Method: zip.Deflate}
	header.SetMode(0o644)
	header.SetModTime(snapshot.Job.UpdatedAt)
	entry, err := zipWriter.CreateHeader(header)
	if err != nil {
		_ = zipWriter.Close()
		return nil, fmt.Errorf("create manifest entry: %w", err)
	}
	if _, err := entry.Write(manifest); err != nil {
		_ = zipWriter.Close()
		return nil, fmt.Errorf("write manifest entry: %w", err)
	}

	if err := zipWriter.Close(); err != nil {
		return nil, fmt.Errorf("finalize ZIP: %w", err)
	}

	return output.Bytes(), nil
}

func encodeArchiveChapters(
	source []archiveFragment,
) ([]encodedArchiveChapter, []archiveManifestEntry, error) {
	if len(source) == 0 {
		return nil, nil, errors.New("audio archive has no fragments")
	}
	fragments := slices.Clone(source)
	slices.SortFunc(fragments, func(left, right archiveFragment) int {
		if byChapter := cmp.Compare(
			left.Resource.ChapterNumber,
			right.Resource.ChapterNumber,
		); byChapter != 0 {
			return byChapter
		}
		if byOrdinal := cmp.Compare(
			left.Resource.Ordinal,
			right.Resource.Ordinal,
		); byOrdinal != 0 {
			return byOrdinal
		}
		return strings.Compare(left.Resource.ID, right.Resource.ID)
	})

	ordinals := make(map[int]string, len(fragments))
	for _, fragment := range fragments {
		if fragment.Resource.Ordinal <= 0 {
			return nil, nil, fmt.Errorf(
				"fragment %q has an invalid ordinal",
				fragment.Resource.ID,
			)
		}
		if previousID, exists := ordinals[fragment.Resource.Ordinal]; exists {
			return nil, nil, fmt.Errorf(
				"fragments %q and %q have duplicate ordinal %d",
				previousID,
				fragment.Resource.ID,
				fragment.Resource.Ordinal,
			)
		}
		ordinals[fragment.Resource.Ordinal] = fragment.Resource.ID
	}

	chapters := make([]encodedArchiveChapter, 0)
	manifestFragments := make([]archiveManifestEntry, 0, len(fragments))
	for start := 0; start < len(fragments); {
		end := start + 1
		number := fragments[start].Resource.ChapterNumber
		for end < len(fragments) &&
			fragments[end].Resource.ChapterNumber == number {
			end++
		}
		chapter, err := encodeArchiveChapter(fragments[start:end])
		if err != nil {
			return nil, nil, fmt.Errorf(
				"prepare chapter %d: %w",
				number,
				err,
			)
		}
		manifestFragments = append(
			manifestFragments,
			chapter.fragments...,
		)
		chapters = append(chapters, chapter)
		start = end
	}
	slices.SortFunc(
		manifestFragments,
		func(left, right archiveManifestEntry) int {
			if byOrdinal := cmp.Compare(left.Ordinal, right.Ordinal); byOrdinal != 0 {
				return byOrdinal
			}
			return strings.Compare(left.ID, right.ID)
		},
	)

	return chapters, manifestFragments, nil
}

func encodeArchiveChapter(
	fragments []archiveFragment,
) (encodedArchiveChapter, error) {
	summary, err := summarizeChapterArchive(fragments)
	if err != nil {
		return encodedArchiveChapter{}, err
	}
	pcmData, format, manifestFragments, err := concatenateChapterPCM(
		fragments,
	)
	if err != nil {
		return encodedArchiveChapter{}, err
	}
	audioFLAC, err := encodePCMAsFLAC(
		pcmData,
		format.sampleRate,
		format.channels,
		format.sampleWidth,
	)
	if err != nil {
		return encodedArchiveChapter{}, fmt.Errorf("encode FLAC: %w", err)
	}
	filename := chapterAudioFilename(summary.Number, summary.Title)
	for index := range manifestFragments {
		manifestFragments[index].Path = filename
	}

	return encodedArchiveChapter{
		manifest: chapterArchiveManifestChapter{
			ChapterNumber:  summary.Number,
			Title:          summary.Title,
			FragmentsCount: summary.FragmentsCount,
			DurationMS:     summary.DurationMS,
			Path:           filename,
			AudioFormat:    "flac",
			SampleRate:     format.sampleRate,
			Channels:       format.channels,
			BitsPerSample:  format.sampleWidth * 8,
			TotalSamples:   format.totalSamples,
			PCMBytes:       int64(len(pcmData)),
		},
		audioFLAC: audioFLAC,
		fragments: manifestFragments,
	}, nil
}

type chapterPCMFormat struct {
	sampleRate   int
	channels     int
	sampleWidth  int
	totalSamples uint64
}

func concatenateChapterPCM(
	fragments []archiveFragment,
) ([]byte, chapterPCMFormat, []archiveManifestEntry, error) {
	if len(fragments) == 0 {
		return nil, chapterPCMFormat{}, nil, errors.New(
			"chapter has no PCM fragments",
		)
	}
	format := chapterPCMFormat{
		sampleRate:  fragments[0].SampleRate,
		channels:    fragments[0].Channels,
		sampleWidth: fragments[0].SampleWidth,
	}
	if err := validateArchiveAudioFormat(format); err != nil {
		return nil, chapterPCMFormat{}, nil, err
	}
	blockAlign := format.channels * format.sampleWidth
	totalBytes := 0
	for _, fragment := range fragments {
		if fragment.SampleRate != format.sampleRate ||
			fragment.Channels != format.channels ||
			fragment.SampleWidth != format.sampleWidth {
			return nil, chapterPCMFormat{}, nil, fmt.Errorf(
				"fragment %q audio format differs from the chapter",
				fragment.Resource.ID,
			)
		}
		if len(fragment.AudioPCM) == 0 ||
			len(fragment.AudioPCM)%blockAlign != 0 {
			return nil, chapterPCMFormat{}, nil, fmt.Errorf(
				"fragment %q PCM is empty or not frame-aligned",
				fragment.Resource.ID,
			)
		}
		if len(fragment.AudioPCM) > int(^uint(0)>>1)-totalBytes {
			return nil, chapterPCMFormat{}, nil, errors.New(
				"chapter PCM size overflows int",
			)
		}
		totalBytes += len(fragment.AudioPCM)
	}

	combined := make([]byte, 0, totalBytes)
	manifest := make([]archiveManifestEntry, 0, len(fragments))
	var startSample uint64
	for _, fragment := range fragments {
		samples := uint64(len(fragment.AudioPCM) / blockAlign)
		combined = append(combined, fragment.AudioPCM...)
		manifest = append(manifest, archiveManifestEntry{
			ID:            fragment.Resource.ID,
			ChapterNumber: fragment.Resource.ChapterNumber,
			Ordinal:       fragment.Resource.Ordinal,
			StartSample:   startSample,
			Samples:       samples,
			Text:          fragment.Resource.Text,
			STTText:       fragment.Resource.STTText,
			Status:        fragment.Resource.Status,
			Attempt:       fragment.Resource.Attempt,
			DurationMS:    fragment.DurationMS,
		})
		startSample += samples
	}
	format.totalSamples = startSample

	return combined, format, manifest, nil
}

func validateArchiveAudioFormat(format chapterPCMFormat) error {
	if format.sampleRate <= 0 || format.sampleRate > 655_350 {
		return fmt.Errorf("unsupported sample rate %d", format.sampleRate)
	}
	if format.channels < 1 || format.channels > 8 {
		return fmt.Errorf("unsupported channel count %d", format.channels)
	}
	if format.sampleWidth != 2 {
		return fmt.Errorf(
			"unsupported sample width %d; only signed 16-bit PCM is exportable",
			format.sampleWidth,
		)
	}
	return nil
}

func encodePCMAsFLAC(
	pcmData []byte,
	sampleRate, channels, sampleWidth int,
) ([]byte, error) {
	format := chapterPCMFormat{
		sampleRate:  sampleRate,
		channels:    channels,
		sampleWidth: sampleWidth,
	}
	if err := validateArchiveAudioFormat(format); err != nil {
		return nil, err
	}
	blockAlign := channels * sampleWidth
	if len(pcmData) == 0 || len(pcmData)%blockAlign != 0 {
		return nil, errors.New("PCM payload is empty or not frame-aligned")
	}
	if len(pcmData)/blockAlign < 16 {
		return nil, errors.New(
			"PCM payload has fewer than 16 samples per channel",
		)
	}

	var output memoryWriteSeeker
	encoder, err := pcm.NewEncoder(&output, pcm.Config{
		SampleRate:       sampleRate,
		BitDepth:         sampleWidth * 8,
		Channels:         channels,
		CompressionLevel: chapterFLACCompressionLevel,
	})
	if err != nil {
		return nil, fmt.Errorf("create FLAC encoder: %w", err)
	}
	if _, err := io.Copy(encoder, bytes.NewReader(pcmData)); err != nil {
		closeErr := encoder.Close()
		return nil, errors.Join(
			fmt.Errorf("write FLAC PCM: %w", err),
			closeErr,
		)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("finalize FLAC encoder: %w", err)
	}
	return output.Bytes(), nil
}

type memoryWriteSeeker struct {
	data   []byte
	offset int64
}

func (w *memoryWriteSeeker) Write(data []byte) (int, error) {
	maxInt := int64(^uint(0) >> 1)
	if w.offset < 0 ||
		w.offset > maxInt ||
		int64(len(data)) > maxInt-w.offset {
		return 0, errors.New("in-memory FLAC output is too large")
	}
	end := int(w.offset) + len(data)
	if end > len(w.data) {
		w.data = append(w.data, make([]byte, end-len(w.data))...)
	}
	copy(w.data[int(w.offset):end], data)
	w.offset = int64(end)
	return len(data), nil
}

func (w *memoryWriteSeeker) Seek(
	offset int64,
	whence int,
) (int64, error) {
	var base int64
	switch whence {
	case io.SeekStart:
		base = 0
	case io.SeekCurrent:
		base = w.offset
	case io.SeekEnd:
		base = int64(len(w.data))
	default:
		return 0, errors.New("invalid seek origin")
	}
	next := base + offset
	if (offset > 0 && next < base) ||
		(offset < 0 && next > base) ||
		next < 0 {
		return 0, errors.New("invalid seek offset")
	}
	w.offset = next
	return next, nil
}

func (w *memoryWriteSeeker) Bytes() []byte {
	return slices.Clone(w.data)
}

func chapterAudioFilename(chapterNumber int, title string) string {
	safeTitle := sanitizeChapterFilenameTitle(title)
	if safeTitle == "" {
		safeTitle = fmt.Sprintf("Глава %d", chapterNumber)
	}
	prefix := fmt.Sprintf("character_%04d_", chapterNumber)
	const suffix = ".flac"
	maxTitleBytes := maxChapterAudioFilenameUTF8Bytes -
		len(prefix) -
		len(suffix)
	safeTitle = strings.TrimRight(
		truncateUTF8Bytes(safeTitle, maxTitleBytes),
		". ",
	)
	if safeTitle == "" {
		safeTitle = "Глава"
	}
	return prefix + safeTitle + suffix
}

func sanitizeChapterFilenameTitle(title string) string {
	title = norm.NFKC.String(title)
	var sanitized strings.Builder
	sanitized.Grow(len(title))
	for _, character := range title {
		switch {
		case unicode.IsControl(character),
			unicode.Is(unicode.Cf, character):
			sanitized.WriteByte(' ')
		case unicode.IsSpace(character):
			sanitized.WriteByte(' ')
		case strings.ContainsRune(`/\<>:"|?*`, character):
			sanitized.WriteByte(' ')
		default:
			sanitized.WriteRune(character)
		}
	}
	title = strings.Trim(strings.Join(strings.Fields(sanitized.String()), " "), ". ")
	runes := []rune(title)
	if len(runes) > maxChapterFilenameTitleRunes {
		title = strings.TrimRight(
			string(runes[:maxChapterFilenameTitleRunes]),
			". ",
		)
	}
	return title
}

func truncateUTF8Bytes(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	if len(value) <= maximum {
		return value
	}
	end := 0
	for index, character := range value {
		size := utf8.RuneLen(character)
		if size < 0 || index+size > maximum {
			break
		}
		end = index + size
	}
	return value[:end]
}

func encodeArchiveManifest(manifest any) ([]byte, error) {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(data, '\n'), nil
}

func summarizeChapterArchive(
	fragments []archiveFragment,
) (chapterSummary, error) {
	if len(fragments) == 0 {
		return chapterSummary{}, errors.New(
			"chapter archive has no fragments",
		)
	}

	first := fragments[0]
	summary := chapterSummary{
		Number: first.Resource.ChapterNumber,
		Title:  first.ChapterTitle,
	}
	if summary.Number <= 0 {
		return chapterSummary{}, errors.New(
			"chapter archive has an invalid chapter number",
		)
	}
	for _, fragment := range fragments {
		if fragment.Resource.ChapterNumber != summary.Number {
			return chapterSummary{}, errors.New(
				"chapter archive contains multiple chapters",
			)
		}
		if fragment.ChapterTitle != summary.Title {
			return chapterSummary{}, errors.New(
				"chapter archive contains inconsistent titles",
			)
		}
		if fragment.DurationMS < 0 {
			return chapterSummary{}, errors.New(
				"chapter archive contains a negative duration",
			)
		}
		summary.FragmentsCount++
		summary.DurationMS += int64(fragment.DurationMS)
	}

	return summary, nil
}

func encodePCMAsWAV(
	pcm []byte,
	sampleRate, channels, sampleWidth int,
) ([]byte, error) {
	if sampleRate <= 0 || channels <= 0 {
		return nil, errors.New("invalid audio dimensions")
	}
	if sampleWidth != 2 {
		return nil, fmt.Errorf("unsupported sample width %d", sampleWidth)
	}
	blockAlign := channels * sampleWidth
	if len(pcm) == 0 || len(pcm)%blockAlign != 0 {
		return nil, errors.New("PCM payload is empty or not frame-aligned")
	}
	if len(pcm) > math.MaxUint32-36 {
		return nil, errors.New("PCM payload is too large for RIFF/WAV")
	}
	byteRate := uint64(sampleRate) * uint64(blockAlign)
	if byteRate > math.MaxUint32 {
		return nil, errors.New("WAV byte rate overflows uint32")
	}

	output := make([]byte, 44+len(pcm))
	copy(output[0:4], "RIFF")
	binary.LittleEndian.PutUint32(output[4:8], uint32(36+len(pcm)))
	copy(output[8:12], "WAVE")
	copy(output[12:16], "fmt ")
	binary.LittleEndian.PutUint32(output[16:20], 16)
	binary.LittleEndian.PutUint16(output[20:22], 1)
	binary.LittleEndian.PutUint16(output[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(output[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(output[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(output[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(output[34:36], uint16(sampleWidth*8))
	copy(output[36:40], "data")
	binary.LittleEndian.PutUint32(output[40:44], uint32(len(pcm)))
	copy(output[44:], pcm)

	return output, nil
}
