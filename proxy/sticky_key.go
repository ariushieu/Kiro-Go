package proxy

import "crypto/sha256"

// stickyKeyFromParts hashes the stable request prefix (system + first user
// message) into a routing key. Billing-header text blocks are stripped so the
// key stays stable across x-anthropic-billing-header drift. Returns the zero
// value when both parts are empty, signalling "no sticky key".
func stickyKeyFromParts(system, firstUser interface{}) [32]byte {
	system = stripBillingBlocks(system)
	firstUser = stripBillingBlocks(firstUser)
	if isEmptyStickyPart(system) && isEmptyStickyPart(firstUser) {
		return [32]byte{}
	}
	hasher := sha256.New()
	writeHashChunk(hasher, canonicalizeCacheValue(system))
	writeHashChunk(hasher, canonicalizeCacheValue(firstUser))
	var key [32]byte
	copy(key[:], hasher.Sum(nil))
	return key
}

// stripBillingBlocks removes x-anthropic-billing-header text blocks from a
// []interface{} content value. Non-array values are returned unchanged.
func stripBillingBlocks(part interface{}) interface{} {
	blocks, ok := part.([]interface{})
	if !ok {
		return part
	}
	out := make([]interface{}, 0, len(blocks))
	for _, b := range blocks {
		if isAnthropicBillingHeaderBlock(b) {
			continue
		}
		out = append(out, b)
	}
	return out
}

func isEmptyStickyPart(part interface{}) bool {
	switch v := part.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []interface{}:
		return len(v) == 0
	default:
		return false
	}
}

func isZeroStickyKey(key [32]byte) bool {
	return key == [32]byte{}
}

func firstClaudeUserContent(messages []ClaudeMessage) interface{} {
	for _, m := range messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return nil
}

func claudeStickyKey(req *ClaudeRequest) [32]byte {
	if req == nil {
		return [32]byte{}
	}
	return stickyKeyFromParts(req.System, firstClaudeUserContent(req.Messages))
}

func firstOpenAIRoleContent(messages []OpenAIMessage, role string) interface{} {
	for _, m := range messages {
		if m.Role == role {
			return m.Content
		}
	}
	return nil
}

func openAIStickyKey(req *OpenAIRequest) [32]byte {
	if req == nil {
		return [32]byte{}
	}
	system := firstOpenAIRoleContent(req.Messages, "system")
	firstUser := firstOpenAIRoleContent(req.Messages, "user")
	return stickyKeyFromParts(system, firstUser)
}
