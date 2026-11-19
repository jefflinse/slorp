package hub

// Launch is what a session is started with.
type Launch struct {
	// Dir is the directory it works in, Name what it is called unless
	// that is empty, and Prompt the first thing said in it.
	Dir, Name, Prompt string
	// Model is the model it runs on and Effort how hard that model works,
	// both in its agent's own words and empty for the agent's usual ones.
	Model, Effort string
	// Local runs it on a local model server in place of its agent's own
	// models, with Model naming one of that server's.
	Local *LocalModel
}

// LocalModel is a local model server a session runs on.
type LocalModel struct {
	// URL is the server's address, which speaks Anthropic's API.
	URL string
	// Thinking says whether the model thinks before it answers.
	Thinking bool
}
