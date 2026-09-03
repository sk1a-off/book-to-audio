# Google AI text preparation pilot for OmniVoice

Date: 2026-08-30  
Method: one deterministic request per relevant language model, followed by a
smaller project-lexicon probe. This is an engineering pilot, not MOS and not a
listening verdict.

No API key, access token, Google project identifier, or full error envelope is
stored in this repository.

## Scope

The API returned 50 model entries. Twenty general language-generation variants
relevant to Russian text preparation were attempted. Image, video, music,
embedding, transcription, realtime audio, native TTS, robotics, computer-use,
and deep-research models were excluded because they do not answer the same
text-rewriting question. Antigravity was tested separately through its required
Interactions agent API.

The model had to:

- keep source order and `А. С. Петров`;
- produce `Глава седьмая`;
- distinguish `за́мок` and `замо́к` without dense stress marks;
- identify the known OmniVoice-specific pronunciation `холма́`;
- expand unambiguous Russian numbers and abbreviations contextually;
- return pauses as metadata, without SSML, `<break>`, `[pause]`, or invented
  inline controls.

## Results

| Model | API result | Technical result |
|---|---|---|
| Gemini 3 Flash Preview | success | Best generic group; correct `за́мок/замо́к`, heading and initials; missed `холма́` |
| Gemini 3.5 Flash | success | Same main result; missed `холма́` |
| Gemini 3.6 Flash | success | Same main result; missed `холма́`; a later probe received temporary 503 |
| Gemini 2.5 Flash | success | Correct homographs, but added unnecessary stress marks to unrelated words and missed `холма́` |
| Gemini 3.5 Flash Lite | success | Reversed both `за́мок/замо́к`; unsuitable without a fail-closed lexicon check |
| Gemini 3.1 Flash Lite | success | Marked only the lock; missed castle and `холма́` |
| Gemini 3.1 Flash Lite Preview | success | Same failure pattern as stable 3.1 Flash Lite |
| Gemini Flash-Lite Latest | success | Wrong/minimal stress, invalid pause anchor, and punctuation loss |
| Gemma 4 31B IT | non-conforming JSON | Usable text inside a code fence, but missed lock and `холма́` |
| Gemma 4 26B A4B IT | invalid output | Degenerated/repetitive JSON output |
| Gemini 2.5 Pro | unavailable | API reports retired for new users |
| Gemini 2.5 Flash-Lite | unavailable | API reports retired for new users |
| Gemini 3.1 Pro Preview | quota blocked | Free-tier limit for this project is zero |
| Gemini 3.1 Pro Preview Custom Tools | quota blocked | Same underlying Pro quota |
| Gemini Pro Latest | quota blocked | Resolved to a Pro quota unavailable to this project |
| Gemini Omni Flash Preview | quota blocked | Free-tier quota unavailable |
| Gemini Omni 1.1 Flash | quota blocked | Free-tier quota unavailable |
| Gemini 3.7 Flash | temporary failure | Repeated 503 high-demand response |
| Gemini Flash Latest | temporary failure | Repeated 503 high-demand response |
| Antigravity Preview | success via agent API | Exact lexicon result, but used a stateful remote agent and 4515 total tokens for a 100-token answer; unjustified for this task |

The first-pass heuristic score was intentionally not retained as a quality
ranking: it falsely rewarded a response that contained both stress spellings in
the wrong positions. Russian/contextual validation must be authoritative.

## Project-lexicon probe

The prompt was then given only these known facts:

- building/fortification: `за́мок`;
- door mechanism: `замо́к`;
- this OmniVoice checkpoint misreads the exact form `холма`, so use
  `холма́`.

Gemini 3 Flash Preview and Gemini 3.5 Flash both returned exactly:

```text
Старинный за́мок стоял на вершине холма́, а дверной замо́к оказался сломан.
```

Antigravity also returned the exact line, but its agent architecture and cost
make it a worse fit. Gemini 3.6 Flash could not complete the second probe due to
a temporary 503, so no lexicon-probe quality claim is made for it.

## Real OmniVoice probe

The local worker was verified as:

```text
k2-fsa/OmniVoice
c5fdb5ccb189668d56333f77ba2629f4cd7535f4+omnivoice-0.2.1
24 kHz mono PCM, voice clone, seed 42, 32 steps, guidance 2.0
```

Three variants were synthesized with only TTS text changed: unmarked, generic
AI (two homograph marks), and AI plus project lexicon (homographs plus
`холма́`). All were 4.48 s, but all three WAV SHA-256 values differ.
This proves that the input marks alter the generated signal; only blind
listening can establish which signal is better.

For the heading boundary, a paragraph break produced 3.56 s versus 3.44 s for
a single space. Faster-Whisper word timestamps estimated the pause between
`седьмая` and `Старинный` at about 260 ms versus 220 ms. This
single sample is a hypothesis-level result: a line break is weak model control,
not a reliable pause-duration API.

## Decision

Use AI only as a bounded contextual selector over deterministic data:

1. preserve canonical text separately;
2. apply deterministic heading/number/abbreviation normalizers where rules are
   reliable;
3. provide a global and book-specific pronunciation lexicon;
4. ask the language model to choose an allowed pronunciation variant from
   context, never to accent every word;
5. validate output fail-closed for text conservation, sparse stress, known
   variants, initials, and unsupported controls;
6. store pause intent as semantic boundary metadata;
7. measure actual generated boundary silence before adding any assembly pause;
8. require blind listening before changing the production default.

Do not add arbitrary SSML or pause tags. The pinned integration passes plain
text to OmniVoice. Upstream documents square-bracket non-verbal and phoneme
controls, but does not document SSML pause control, and public stress issues do
not establish U+0301 as a stable supported Russian API contract. In this project
U+0301 remains an experimentally validated, checkpoint-specific behavior.

## Blind listener result

The listener selected `A` in the stress A/B/C package. Reveal mapping:

- `A`: AI plus project lexicon;
- `B`: raw unmarked text;
- `C`: generic AI without the project lexicon.

The accepted baseline is therefore sparse AI plus a deterministic global/book
lexicon. This is evidence for the exact tested sentence and checkpoint, not a
claim that arbitrary LLM accentuation is safe.

For the heading pause, the listener rated both `D` (paragraph break) and `E`
(single space) as lacking a normal pause. Text whitespace is rejected as the
pause-control mechanism. Inspection of the production path found that FB2
block boundaries are flattened into `[]string`, and chapter export concatenates
fragment PCM byte-for-byte. A correct implementation must preserve boundary
type through parsing, persistence and export, then enforce a boundary-specific
minimum pause after measuring existing trailing and leading silence.
