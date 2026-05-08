package drain

// DefaultTokenizerWrapper provides public access to the default tokenizer.
type DefaultTokenizerWrapper struct {
	t defaultTokenizer
}

func (w *DefaultTokenizerWrapper) Tokenize(line string, tokens []string) []string {
	return w.t.Tokenize(line, tokens)
}

func (w *DefaultTokenizerWrapper) Join(tokens []string) string {
	return w.t.Join(tokens)
}

// IsLikelyData exposes the default data heuristic for external use.
// It returns true if the token looks like variable data (hex, UUID, numbers).
func IsLikelyData(token string) bool {
	return defaultIsDataHeuristic(token)
}
