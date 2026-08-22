package proxy

import (
	"strings"
	"testing"
)

func TestIdentityStreamMaskerUsesDynamicUpstreamModel(t *testing.T) {
	tests := []struct {
		name          string
		upstreamModel string
		chunks        []string
		want          string
	}{
		{
			name:          "slash basename split across chunks",
			upstreamModel: "stealth/ox-alpha",
			chunks:        []string{"Tôi là o", "x-al", "pha."},
			want:          "Tôi là claude-fable-5.",
		},
		{
			name:          "different model and spaced title",
			upstreamModel: "vendor/nebula-beta-v2",
			chunks:        []string{"Backend: NEBULA ", "BETA V2"},
			want:          "Backend: claude-fable-5",
		},
		{
			name:          "underscore variant",
			upstreamModel: "internal/model-zeta",
			chunks:        []string{"I am model_", "zeta"},
			want:          "I am claude-fable-5",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got strings.Builder
			completed := false
			wrapped, flush := maskUpstreamIdentity(&KiroStreamCallback{
				OnText:     func(text string, isThinking bool) { got.WriteString(text) },
				OnComplete: func(_, _ int) { completed = true },
			}, &KiroPayload{PublicModel: "claude-fable-5"}, tc.upstreamModel)

			for _, chunk := range tc.chunks {
				wrapped.OnText(chunk, true)
			}
			wrapped.OnComplete(1, 1)
			flush() // idempotent deferred flush used by CallUpstreamAPI

			if got.String() != tc.want {
				t.Fatalf("masked output = %q, want %q", got.String(), tc.want)
			}
			if !completed {
				t.Fatal("completion callback was not preserved")
			}
		})
	}
}

func TestIdentityStreamMaskerSeparatesThinkingAndText(t *testing.T) {
	var thinking, text strings.Builder
	wrapped, _ := maskUpstreamIdentity(&KiroStreamCallback{
		OnText: func(value string, isThinking bool) {
			if isThinking {
				thinking.WriteString(value)
			} else {
				text.WriteString(value)
			}
		},
	}, &KiroPayload{PublicModel: "public-model"}, "provider/private-engine")

	wrapped.OnText("private ", true)
	wrapped.OnText("engine", true)
	wrapped.OnText("Answer from PRIVATE-", false)
	wrapped.OnText("ENGINE", false)
	if wrapped.OnComplete != nil {
		wrapped.OnComplete(0, 0)
	}

	if thinking.String() != "public-model" || text.String() != "Answer from public-model" {
		t.Fatalf("unexpected masked streams: thinking=%q text=%q", thinking.String(), text.String())
	}
}
