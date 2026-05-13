package salute

import (
	"encoding/json"
	"strings"
	"testing"
)

// Stereo example from SaluteSpeech async gRPC docs: two channels, identical hypothesis text and timing.
const saluteStereoDupJSON = `[
  {
    "results": [
      {
        "text": "раз два три",
        "normalized_text": "1 2 3",
        "start": "0.760s",
        "end": "2s",
        "word_alignments": [
          {"word": "раз", "start": "0.760s", "end": "0.880s"},
          {"word": "два", "start": "1.340s", "end": "1.460s"},
          {"word": "три", "start": "1.880s", "end": "2s"}
        ]
      }
    ],
    "eou": true,
    "processed_audio_start": "0s",
    "processed_audio_end": "2.447375104s",
    "channel": 0
  },
  {
    "results": [
      {
        "text": "раз два три",
        "normalized_text": "1 2 3",
        "start": "0.760s",
        "end": "2s",
        "word_alignments": [
          {"word": "раз", "start": "0.760s", "end": "0.880s"},
          {"word": "два", "start": "1.340s", "end": "1.460s"},
          {"word": "три", "start": "1.880s", "end": "2s"}
        ]
      }
    ],
    "eou": true,
    "processed_audio_start": "0s",
    "processed_audio_end": "2.447375104s",
    "channel": 1
  }
]`

// Speaker-separation excerpt from the same documentation: mixed speaker_id -1 row must not surface;
// partial hypotheses with eou=false should be dropped when an eou=true row exists for the same buffer and speaker.
const saluteDiarizationJSON = `[
  {
    "results": [
      {
        "text": "один два",
        "normalized_text": "1 2",
        "start": "0.320s",
        "end": "0.960s",
        "word_alignments": [
          {"word": "один", "start": "0.320s", "end": "0.519999968s"},
          {"word": "два", "start": "0.640s", "end": "0.960s"}
        ]
      }
    ],
    "eou": false,
    "processed_audio_start": "0s",
    "processed_audio_end": "2.444875008s",
    "channel": 0,
    "speaker_info": {"speaker_id": 2, "main_speaker_confidence": 0.31}
  },
  {
    "results": [
      {
        "text": "один два четыре пять шесть",
        "normalized_text": "1, 3, 4, 5 6.",
        "start": "0.320s",
        "end": "1.880s",
        "word_alignments": [
          {"word": "один", "start": "0.320s", "end": "0.500s"},
          {"word": "два", "start": "0.780s", "end": "0.880s"},
          {"word": "четыре", "start": "1.160s", "end": "1.380s"},
          {"word": "пять", "start": "1.480s", "end": "1.620s"},
          {"word": "шесть", "start": "1.700s", "end": "1.880s"}
        ]
      }
    ],
    "eou": false,
    "processed_audio_start": "0s",
    "processed_audio_end": "2.444875008s",
    "channel": 0,
    "speaker_info": {"speaker_id": -1, "main_speaker_confidence": 0.31}
  },
  {
    "results": [
      {
        "text": "четыре пять шесть",
        "normalized_text": "4, 5 6.",
        "start": "1.140s",
        "end": "1.900s",
        "word_alignments": [
          {"word": "четыре", "start": "1.140s", "end": "1.400s"},
          {"word": "пять", "start": "1.480s", "end": "1.620s"},
          {"word": "шесть", "start": "1.680s", "end": "1.900s"}
        ]
      }
    ],
    "eou": true,
    "processed_audio_start": "0s",
    "processed_audio_end": "2.444875008s",
    "channel": 0,
    "speaker_info": {"speaker_id": 1, "main_speaker_confidence": 0.37}
  }
]`

func TestExtractSegments_StereoDuplicateChannels(t *testing.T) {
	var raw any
	if err := json.Unmarshal([]byte(saluteStereoDupJSON), &raw); err != nil {
		t.Fatal(err)
	}
	segs := extractSegments(raw)
	if len(segs) != 1 {
		t.Fatalf("expected 1 segment after stereo dedupe, got %d: %+v", len(segs), segs)
	}
	if segs[0].Speaker != "Спикер 1" {
		t.Fatalf("expected lowest channel to win, speaker=%q", segs[0].Speaker)
	}
}

func TestExtractSegments_SpeakerSeparationSkipsMixedAndPartial(t *testing.T) {
	var raw any
	if err := json.Unmarshal([]byte(saluteDiarizationJSON), &raw); err != nil {
		t.Fatal(err)
	}
	segs := extractSegments(raw)
	var speakers []string
	for _, s := range segs {
		speakers = append(speakers, s.Speaker)
	}
	if strings.Contains(strings.Join(speakers, ","), "-1") {
		t.Fatalf("mixed speaker_id -1 must be skipped: %v", speakers)
	}
	hasSp1 := false
	hasSp2 := false
	for _, s := range segs {
		switch s.Speaker {
		case "Спикер 1":
			hasSp1 = true
		case "Спикер 2":
			hasSp2 = true
		}
	}
	if !hasSp1 || !hasSp2 {
		t.Fatalf("expected both Спикер 1 and Спикер 2, got %v segments=%+v", speakers, segs)
	}
	for _, s := range segs {
		if strings.Contains(s.Text, "один два четыре пять шесть") {
			t.Fatalf("mixed long hypothesis should be removed: %q", s.Text)
		}
	}
}

func TestSpeakerDisplayFromNode_ChannelZero(t *testing.T) {
	label, skip := speakerDisplayFromNode(map[string]any{
		"channel": float64(0),
		"results": []any{},
	})
	if skip {
		t.Fatal("unexpected skip")
	}
	if label != "Спикер 1" {
		t.Fatalf("channel 0 must map to Спикер 1, got %q", label)
	}
	label2, _ := speakerDisplayFromNode(map[string]any{
		"channel": float64(1),
		"results": []any{},
	})
	if label2 != "Спикер 2" {
		t.Fatalf("channel 1 must map to Спикер 2, got %q", label2)
	}
}
