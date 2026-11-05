package executor

import cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"

// ForAPIKey binds configuration once after credential selection. Value receivers
// copy executor configuration while preserving shared transport/session stores.
func (e GeminiVertexExecutor) ForAPIKey() cliproxyauth.ProviderExecutor {
	e.cfg = e.cfg.ForAPIKey()
	return &e
}

func (e XAIWebsocketsExecutor) ForAPIKey() cliproxyauth.ProviderExecutor {
	if e.XAIExecutor != nil {
		e.XAIExecutor = e.XAIExecutor.ForAPIKey().(*XAIExecutor)
	}
	return &e
}
