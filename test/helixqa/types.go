// Package helixqa is a data-driven test bank for
// translate.AnthropicToOpenAI: the bank files under banks/*.json declare
// cases (an Anthropic-shaped input request, plus the expected OpenAI
// translation or expected error), and runner_test.go executes every one of
// them. Adding a new case, or a whole new banks/*.json file, requires no Go
// code change -- see README.md.
//
// The name intentionally advertises the ecosystem-standard "HelixQA
// test-bank" shape (bank files keyed by "test_cases", loadable by
// digital.vasic.challenges' pkg/bank), even though this package does not
// import that module: see README.md "Why not import digital.vasic.challenges
// directly" for why this suite is a small, self-contained, hermetic
// reimplementation of just the parts it needs.
package helixqa

import "encoding/json"

// OptionsSpec mirrors the subset of translate.Options a bank case can drive,
// plus one extra knob that models the real two-stage pipeline: a provider
// with the "cleancache" transformer strips cache_control from the raw
// request bytes (translate.StripCacheControl) BEFORE the Anthropic->OpenAI
// conversion ever runs. StripCacheFirst lets a case exercise that exact
// composition instead of only the conversion in isolation.
type OptionsSpec struct {
	CleanCache           bool   `json:"clean_cache,omitempty"`
	StreamOptions        bool   `json:"stream_options,omitempty"`
	EnsureToolParameters bool   `json:"ensure_tool_parameters,omitempty"`
	Model                string `json:"model,omitempty"`
	StripCacheFirst      bool   `json:"strip_cache_control_first,omitempty"`
}

// MessageExpect is a partial (subset) assertion against one output message,
// matched positionally against translate.OpenAIRequest.Messages[i]. Every
// field is optional: a case only asserts what it cares about.
type MessageExpect struct {
	Role                      *string  `json:"role,omitempty"`
	Content                   *string  `json:"content,omitempty"`
	ContentContains           *string  `json:"content_contains,omitempty"`
	ContentNull               *bool    `json:"content_null,omitempty"`
	ToolCallsCount            *int     `json:"tool_calls_count,omitempty"`
	ToolCallNames             []string `json:"tool_call_names,omitempty"`
	ToolCallArgumentsContains *string  `json:"tool_call_arguments_contains,omitempty"`
	ToolCallID                *string  `json:"tool_call_id,omitempty"`
}

// ToolExpect is a partial assertion against one output tool definition,
// matched positionally against translate.OpenAIRequest.Tools[i].
type ToolExpect struct {
	Name             *string `json:"name,omitempty"`
	Description      *string `json:"description,omitempty"`
	ParametersObject *bool   `json:"parameters_object,omitempty"`
	ParametersAbsent *bool   `json:"parameters_absent,omitempty"`
}

// Expect is the full set of optional, subset-match assertions a case can
// make against a successful translate.AnthropicToOpenAI result. Nil/omitted
// fields are not checked at all -- there is deliberately no way to assert
// "and nothing else", so cases stay focused on the behaviour they document
// instead of over-specifying the whole payload.
type Expect struct {
	Model                     *string         `json:"model,omitempty"`
	MaxTokens                 *int            `json:"max_tokens,omitempty"`
	Stream                    *bool           `json:"stream,omitempty"`
	Temperature               *float64        `json:"temperature,omitempty"`
	TemperatureNull           *bool           `json:"temperature_null,omitempty"`
	TopP                      *float64        `json:"top_p,omitempty"`
	TopPNull                  *bool           `json:"top_p_null,omitempty"`
	Stop                      *[]string       `json:"stop,omitempty"`
	StreamOptionsIncludeUsage *bool           `json:"stream_options_include_usage,omitempty"`
	StreamOptionsNull         *bool           `json:"stream_options_null,omitempty"`
	MessagesCount             *int            `json:"messages_count,omitempty"`
	Messages                  []MessageExpect `json:"messages,omitempty"`
	ToolsCount                *int            `json:"tools_count,omitempty"`
	Tools                     []ToolExpect    `json:"tools,omitempty"`
}

// Case is one declarative bank test case: an Anthropic-shaped Input, the
// Options to convert it with, and either ExpectError (+ optional
// ErrorContains substring) or Expect (a subset match against the resulting
// OpenAI request).
type Case struct {
	ID            string          `json:"id"`
	Description   string          `json:"description"`
	Category      string          `json:"category"`
	Tags          []string        `json:"tags,omitempty"`
	Options       OptionsSpec     `json:"options,omitempty"`
	Input         json.RawMessage `json:"input"`
	ExpectError   bool            `json:"expect_error"`
	ErrorContains string          `json:"error_contains,omitempty"`
	Expect        *Expect         `json:"expect,omitempty"`
}

// Bank is the top-level shape of a banks/*.json file.
type Bank struct {
	Version     string `json:"version"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	TestCases   []Case `json:"test_cases"`
}
