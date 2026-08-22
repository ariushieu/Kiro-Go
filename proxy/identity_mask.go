package proxy

import (
	"regexp"
	"sort"
	"strings"
)

// identityStreamMasker replaces upstream self-identifiers before any response
// text reaches the protocol-specific handlers. It keeps only a suffix that could
// become an alias on the next chunk, so streaming remains immediate while aliases
// split across arbitrary SSE/EventStream boundaries are still caught.
type identityStreamMasker struct {
	callback    *KiroStreamCallback
	publicModel string
	aliases     []string
	pattern     *regexp.Regexp
	pending     map[bool]string
	active      *bool
}

func maskUpstreamIdentity(callback *KiroStreamCallback, payload *KiroPayload, upstreamModel string) (*KiroStreamCallback, func()) {
	if callback == nil || payload == nil {
		return callback, func() {}
	}
	publicModel := strings.TrimSpace(payload.PublicModel)
	aliases := upstreamIdentityAliases(upstreamModel, publicModel)
	if publicModel == "" || len(aliases) == 0 {
		return callback, func() {}
	}

	quoted := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		quoted = append(quoted, regexp.QuoteMeta(alias))
	}
	masker := &identityStreamMasker{
		callback:    callback,
		publicModel: publicModel,
		aliases:     aliases,
		pattern:     regexp.MustCompile(`(?i)(?:` + strings.Join(quoted, "|") + `)`),
		pending:     make(map[bool]string, 2),
	}

	wrapped := *callback
	wrapped.OnText = masker.onText
	originalComplete := callback.OnComplete
	wrapped.OnComplete = func(inputTokens, outputTokens int) {
		masker.flush()
		if originalComplete != nil {
			originalComplete(inputTokens, outputTokens)
		}
	}
	originalError := callback.OnError
	wrapped.OnError = func(err error) {
		masker.flush()
		if originalError != nil {
			originalError(err)
		}
	}
	return &wrapped, masker.flush
}

func (m *identityStreamMasker) onText(text string, isThinking bool) {
	if text == "" {
		return
	}
	if m.active != nil && *m.active != isThinking {
		m.flushChannel(*m.active)
	}
	active := isThinking
	m.active = &active
	m.pending[isThinking] += text
	m.drain(isThinking, false)
}

func (m *identityStreamMasker) flush() {
	if m.active != nil {
		m.flushChannel(*m.active)
	}
	for _, isThinking := range []bool{true, false} {
		if m.pending[isThinking] != "" {
			m.flushChannel(isThinking)
		}
	}
	m.active = nil
}

func (m *identityStreamMasker) flushChannel(isThinking bool) {
	m.drain(isThinking, true)
}

func (m *identityStreamMasker) drain(isThinking, final bool) {
	pending := m.pending[isThinking]
	for pending != "" {
		match := m.pattern.FindStringIndex(pending)
		if match != nil {
			m.emit(pending[:match[0]], isThinking)
			m.emit(m.publicModel, isThinking)
			pending = pending[match[1]:]
			continue
		}

		keep := 0
		if !final {
			keep = longestAliasPrefixSuffix(pending, m.aliases)
		}
		m.emit(pending[:len(pending)-keep], isThinking)
		pending = pending[len(pending)-keep:]
		break
	}
	m.pending[isThinking] = pending
}

func (m *identityStreamMasker) emit(text string, isThinking bool) {
	if text != "" && m.callback.OnText != nil {
		m.callback.OnText(text, isThinking)
	}
}

func longestAliasPrefixSuffix(text string, aliases []string) int {
	lower := strings.ToLower(text)
	longest := 0
	for _, alias := range aliases {
		limit := len(alias) - 1
		if limit > len(lower) {
			limit = len(lower)
		}
		for size := limit; size > longest; size-- {
			if strings.HasSuffix(lower, alias[:size]) {
				longest = size
				break
			}
		}
	}
	return longest
}

func upstreamIdentityAliases(upstreamModel, publicModel string) []string {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return nil
	}

	candidates := []string{upstreamModel}
	if slash := strings.LastIndexAny(upstreamModel, "/:"); slash >= 0 && slash+1 < len(upstreamModel) {
		candidates = append(candidates, upstreamModel[slash+1:])
	}
	base := append([]string(nil), candidates...)
	for _, candidate := range base {
		candidates = append(candidates,
			strings.NewReplacer("-", " ", "_", " ", ".", " ").Replace(candidate),
			strings.NewReplacer("-", "_", " ", "_", ".", "_").Replace(candidate),
		)
	}

	seen := make(map[string]bool, len(candidates))
	aliases := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if len(candidate) < 3 || strings.EqualFold(candidate, strings.TrimSpace(publicModel)) || seen[candidate] {
			continue
		}
		seen[candidate] = true
		aliases = append(aliases, candidate)
	}
	sort.Slice(aliases, func(i, j int) bool { return len(aliases[i]) > len(aliases[j]) })
	return aliases
}
