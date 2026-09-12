package config

// Selectors match native UIA outline fields exactly, never transient look refs.
type Selector struct {
	Role        string `json:"role,omitempty"`
	Identifier  string `json:"identifier,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}
type WeCom struct {
	Helper         string   `json:"helper,omitempty"`
	App            string   `json:"app"`
	WindowTitle    string   `json:"window_title"`
	ChatTitle      string   `json:"chat_title"`
	Chat           Selector `json:"chat"`
	Messages       Selector `json:"messages"`
	Row            Selector `json:"row"`
	Input          Selector `json:"input"`
	Send           Selector `json:"send"`
	MessagePattern string   `json:"message_pattern"`
	Peer           string   `json:"peer"`
	Self           string   `json:"self"`
	PollMS         int      `json:"poll_ms,omitempty"`
}
