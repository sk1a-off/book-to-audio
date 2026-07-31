package api

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tphakala/go-flac/pcm"
)

func TestBuildAudioZIPContainsMergedLosslessFLACChapters(t *testing.T) {
	archive, err := buildAudioZIP(chapterArchiveTestSnapshot())
	if err != nil {
		t.Fatalf("buildAudioZIP() error = %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}
	names := make([]string, 0, len(reader.File))
	files := make(map[string][]byte, len(reader.File))
	for _, file := range reader.File {
		names = append(names, file.Name)
		entry, err := file.Open()
		if err != nil {
			t.Fatalf("open ZIP entry %q: %v", file.Name, err)
		}
		data, err := io.ReadAll(entry)
		_ = entry.Close()
		if err != nil {
			t.Fatalf("read ZIP entry %q: %v", file.Name, err)
		}
		files[file.Name] = data
	}
	wantNames := []string{
		"character_0001_Начало.flac",
		"character_0002_Продолжение.flac",
		"manifest.json",
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("ZIP names = %q, want %q", names, wantNames)
	}

	assertArchiveTestFLAC(
		t,
		files["character_0001_Начало.flac"],
		append(
			chapterArchiveTestPCM(1),
			chapterArchiveTestPCM(3)...,
		),
		32,
	)
	assertArchiveTestFLAC(
		t,
		files["character_0002_Продолжение.flac"],
		chapterArchiveTestPCM(2),
		16,
	)

	manifestData := archiveTestManifest(t, reader)
	var manifest archiveManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatalf("decode full manifest: %v", err)
	}
	if manifest.Version != fullArchiveManifestVersion ||
		len(manifest.Chapters) != 2 ||
		len(manifest.Fragments) != 3 {
		t.Errorf("full manifest = %+v", manifest)
	}
	if manifest.Chapters[0].Path != "character_0001_Начало.flac" ||
		manifest.Chapters[0].AudioFormat != "flac" ||
		manifest.Chapters[0].SampleRate != 24_000 ||
		manifest.Chapters[0].Channels != 1 ||
		manifest.Chapters[0].BitsPerSample != 16 ||
		manifest.Chapters[0].TotalSamples != 32 ||
		manifest.Chapters[0].PCMBytes != 64 {
		t.Errorf("first chapter manifest = %+v", manifest.Chapters[0])
	}
	if manifest.Fragments[0].Ordinal != 1 ||
		manifest.Fragments[0].Path != manifest.Chapters[0].Path ||
		manifest.Fragments[0].StartSample != 0 ||
		manifest.Fragments[0].Samples != 16 ||
		manifest.Fragments[1].Ordinal != 2 ||
		manifest.Fragments[1].Path != manifest.Chapters[1].Path ||
		manifest.Fragments[1].StartSample != 0 ||
		manifest.Fragments[2].Ordinal != 3 ||
		manifest.Fragments[2].Path != manifest.Chapters[0].Path ||
		manifest.Fragments[2].StartSample != 16 {
		t.Errorf("fragment manifest boundaries = %+v", manifest.Fragments)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(manifestData, &fields); err != nil {
		t.Fatalf("decode full manifest fields: %v", err)
	}
	if _, exists := fields["scope"]; exists {
		t.Error("backward-compatible full v1 manifest unexpectedly has scope")
	}
	if _, exists := fields["chapter"]; exists {
		t.Error("full manifest unexpectedly has singular chapter scope")
	}
}

func TestBuildAudioZIPIsDeterministic(t *testing.T) {
	snapshot := chapterArchiveTestSnapshot()
	first, err := buildAudioZIP(snapshot)
	if err != nil {
		t.Fatalf("first buildAudioZIP() error = %v", err)
	}
	second, err := buildAudioZIP(snapshot)
	if err != nil {
		t.Fatalf("second buildAudioZIP() error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("full FLAC ZIP differs for the same immutable snapshot")
	}
}

func TestBuildChapterAudioZIPRejectsMixedChapterSnapshot(t *testing.T) {
	if _, err := buildChapterAudioZIP(
		chapterArchiveTestSnapshot(),
	); err == nil {
		t.Fatal("buildChapterAudioZIP(mixed chapters) error = nil")
	}
}

func TestEncodePCMAsFLACRoundTripsAndFinalizesStreamInfo(t *testing.T) {
	pcmData := chapterArchiveTestPCM(7)
	flac, err := encodePCMAsFLAC(pcmData, 24_000, 1, 2)
	if err != nil {
		t.Fatalf("encodePCMAsFLAC() error = %v", err)
	}
	assertArchiveTestFLAC(t, flac, pcmData, 16)
}

func TestBuildAudioZIPRejectsInvalidChapterPCM(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*archiveSnapshot)
	}{
		{
			name: "mismatched sample rate",
			mutate: func(snapshot *archiveSnapshot) {
				snapshot.Fragments[1].SampleRate = 16_000
			},
		},
		{
			name: "mismatched channels",
			mutate: func(snapshot *archiveSnapshot) {
				snapshot.Fragments[1].Channels = 2
			},
		},
		{
			name: "unsupported sample width",
			mutate: func(snapshot *archiveSnapshot) {
				snapshot.Fragments[2].SampleWidth = 1
			},
		},
		{
			name: "empty PCM",
			mutate: func(snapshot *archiveSnapshot) {
				snapshot.Fragments[1].AudioPCM = nil
			},
		},
		{
			name: "unaligned PCM",
			mutate: func(snapshot *archiveSnapshot) {
				snapshot.Fragments[1].AudioPCM = []byte{1}
			},
		},
		{
			name: "chapter is too short for interoperable FLAC",
			mutate: func(snapshot *archiveSnapshot) {
				snapshot.Fragments[1].AudioPCM = []byte{1, 0}
				snapshot.Fragments[2].AudioPCM = []byte{2, 0}
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			snapshot := chapterArchiveTestSnapshot()
			test.mutate(&snapshot)
			if _, err := buildAudioZIP(snapshot); err == nil {
				t.Fatal("buildAudioZIP() error = nil")
			}
		})
	}
}

func TestChapterAudioFilenameSanitizesAndBoundsUTF8(t *testing.T) {
	t.Parallel()

	if got := chapterAudioFilename(
		1,
		"Глава 1 Прибытие",
	); got != "character_0001_Глава 1 Прибытие.flac" {
		t.Fatalf("exact filename = %q", got)
	}
	if got := chapterAudioFilename(
		1,
		"../Глава\\ 1:\x00 <Прибытие>?*|",
	); got != "character_0001_Глава 1 Прибытие.flac" {
		t.Errorf("sanitized filename = %q", got)
	}
	if got := chapterAudioFilename(
		2,
		" \t.\u202e ",
	); got != "character_0002_Глава 2.flac" {
		t.Errorf("fallback filename = %q", got)
	}

	long := chapterAudioFilename(9, strings.Repeat("Я", 200))
	if len(long) > maxChapterAudioFilenameUTF8Bytes ||
		!utf8.ValidString(long) ||
		!strings.HasSuffix(long, ".flac") {
		t.Errorf(
			"bounded UTF-8 filename bytes=%d valid=%v value=%q",
			len(long),
			utf8.ValidString(long),
			long,
		)
	}
	if strings.ContainsAny(long, `/\`) {
		t.Errorf("filename contains a path separator: %q", long)
	}
}

func assertArchiveTestFLAC(
	t *testing.T,
	data, wantPCM []byte,
	wantSamples uint64,
) {
	t.Helper()
	if len(data) < 4 || string(data[:4]) != "fLaC" {
		t.Fatalf("FLAC signature is missing: %x", data)
	}
	decoder, err := pcm.NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("pcm.NewDecoder() error = %v", err)
	}
	info := decoder.Info()
	if info.SampleRate != 24_000 ||
		info.Channels != 1 ||
		info.BitDepth != 16 ||
		info.TotalSamples != wantSamples {
		t.Errorf("FLAC STREAMINFO = %+v", info)
	}
	decoded, err := io.ReadAll(decoder)
	if err != nil {
		t.Fatalf("decode FLAC: %v", err)
	}
	if !bytes.Equal(decoded, wantPCM) {
		t.Fatalf("decoded PCM = %v, want %v", decoded, wantPCM)
	}
}

func archiveTestManifest(t *testing.T, reader *zip.Reader) []byte {
	t.Helper()
	for _, file := range reader.File {
		if file.Name != "manifest.json" {
			continue
		}
		entry, err := file.Open()
		if err != nil {
			t.Fatalf("open manifest: %v", err)
		}
		data, err := io.ReadAll(entry)
		_ = entry.Close()
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		return data
	}
	t.Fatal("manifest.json not found")
	return nil
}

func TestEncodePCMAsWAVHeaderAndPayload(t *testing.T) {
	pcm := []byte{0x00, 0x00, 0x10, 0x00, 0xf0, 0xff, 0x00, 0x00}

	wav, err := encodePCMAsWAV(pcm, 24_000, 1, 2)
	if err != nil {
		t.Fatalf("encodePCMAsWAV() error = %v", err)
	}
	if len(wav) != 44+len(pcm) {
		t.Fatalf("WAV length = %d, want %d", len(wav), 44+len(pcm))
	}
	if string(wav[0:4]) != "RIFF" ||
		string(wav[8:12]) != "WAVE" ||
		string(wav[12:16]) != "fmt " ||
		string(wav[36:40]) != "data" {
		t.Errorf("WAV chunk identifiers are invalid")
	}
	if got := binary.LittleEndian.Uint32(wav[4:8]); got !=
		uint32(36+len(pcm)) {
		t.Errorf("RIFF chunk size = %d", got)
	}
	if got := binary.LittleEndian.Uint16(wav[20:22]); got != 1 {
		t.Errorf("audio format = %d, want PCM", got)
	}
	if got := binary.LittleEndian.Uint16(wav[22:24]); got != 1 {
		t.Errorf("channels = %d", got)
	}
	if got := binary.LittleEndian.Uint32(wav[24:28]); got != 24_000 {
		t.Errorf("sample rate = %d", got)
	}
	if got := binary.LittleEndian.Uint32(wav[28:32]); got != 48_000 {
		t.Errorf("byte rate = %d", got)
	}
	if got := binary.LittleEndian.Uint16(wav[32:34]); got != 2 {
		t.Errorf("block alignment = %d", got)
	}
	if got := binary.LittleEndian.Uint16(wav[34:36]); got != 16 {
		t.Errorf("bits per sample = %d", got)
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got !=
		uint32(len(pcm)) {
		t.Errorf("data size = %d", got)
	}
	if !bytes.Equal(wav[44:], pcm) {
		t.Errorf("WAV payload = %v, want %v", wav[44:], pcm)
	}
}

func TestEncodePCMAsWAVRejectsInvalidAudio(t *testing.T) {
	tests := []struct {
		name        string
		pcm         []byte
		sampleRate  int
		channels    int
		sampleWidth int
	}{
		{
			name:        "empty PCM",
			sampleRate:  24_000,
			channels:    1,
			sampleWidth: 2,
		},
		{
			name:        "unaligned PCM",
			pcm:         []byte{1},
			sampleRate:  24_000,
			channels:    1,
			sampleWidth: 2,
		},
		{
			name:        "invalid sample rate",
			pcm:         []byte{0, 0},
			sampleRate:  0,
			channels:    1,
			sampleWidth: 2,
		},
		{
			name:        "invalid channels",
			pcm:         []byte{0, 0},
			sampleRate:  24_000,
			channels:    0,
			sampleWidth: 2,
		},
		{
			name:        "unsupported sample width",
			pcm:         []byte{0, 0},
			sampleRate:  24_000,
			channels:    1,
			sampleWidth: 1,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			_, err := encodePCMAsWAV(
				test.pcm,
				test.sampleRate,
				test.channels,
				test.sampleWidth,
			)
			if err == nil {
				t.Fatal("encodePCMAsWAV() error = nil")
			}
		})
	}
}
