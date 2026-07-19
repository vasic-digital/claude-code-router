package helixqa

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vasic-digital/claude-code-router/internal/translate"
)

// matchExpect checks out against every assertion actually present in exp,
// returning one human-readable mismatch per failed assertion. An empty
// result means the case passed.
func matchExpect(out *translate.OpenAIRequest, exp *Expect) []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if exp.Model != nil && out.Model != *exp.Model {
		add("model = %q, want %q", out.Model, *exp.Model)
	}
	if exp.MaxTokens != nil && out.MaxTokens != *exp.MaxTokens {
		add("max_tokens = %d, want %d", out.MaxTokens, *exp.MaxTokens)
	}
	if exp.Stream != nil && out.Stream != *exp.Stream {
		add("stream = %v, want %v", out.Stream, *exp.Stream)
	}
	if exp.Temperature != nil {
		if out.Temperature == nil {
			add("temperature = nil, want %v", *exp.Temperature)
		} else if *out.Temperature != *exp.Temperature {
			add("temperature = %v, want %v", *out.Temperature, *exp.Temperature)
		}
	}
	if exp.TemperatureNull != nil && *exp.TemperatureNull && out.Temperature != nil {
		add("temperature = %v, want nil", *out.Temperature)
	}
	if exp.TopP != nil {
		if out.TopP == nil {
			add("top_p = nil, want %v", *exp.TopP)
		} else if *out.TopP != *exp.TopP {
			add("top_p = %v, want %v", *out.TopP, *exp.TopP)
		}
	}
	if exp.TopPNull != nil && *exp.TopPNull && out.TopP != nil {
		add("top_p = %v, want nil", *out.TopP)
	}
	if exp.Stop != nil {
		want := *exp.Stop
		if len(out.Stop) != len(want) {
			add("stop = %v, want %v", out.Stop, want)
		} else {
			for i := range want {
				if out.Stop[i] != want[i] {
					add("stop[%d] = %q, want %q", i, out.Stop[i], want[i])
				}
			}
		}
	}
	if exp.StreamOptionsIncludeUsage != nil {
		if out.StreamOptions == nil {
			add("stream_options = nil, want include_usage=%v", *exp.StreamOptionsIncludeUsage)
		} else if out.StreamOptions.IncludeUsage != *exp.StreamOptionsIncludeUsage {
			add("stream_options.include_usage = %v, want %v", out.StreamOptions.IncludeUsage, *exp.StreamOptionsIncludeUsage)
		}
	}
	if exp.StreamOptionsNull != nil && *exp.StreamOptionsNull && out.StreamOptions != nil {
		add("stream_options present, want nil (non-streaming requests must not carry it)")
	}
	if exp.MessagesCount != nil && len(out.Messages) != *exp.MessagesCount {
		add("messages count = %d, want %d", len(out.Messages), *exp.MessagesCount)
	}
	for i, me := range exp.Messages {
		if i >= len(out.Messages) {
			add("messages[%d]: expected but only %d message(s) present", i, len(out.Messages))
			continue
		}
		errs = append(errs, matchMessage(out.Messages[i], me, i)...)
	}
	if exp.ToolsCount != nil && len(out.Tools) != *exp.ToolsCount {
		add("tools count = %d, want %d", len(out.Tools), *exp.ToolsCount)
	}
	for i, te := range exp.Tools {
		if i >= len(out.Tools) {
			add("tools[%d]: expected but only %d tool(s) present", i, len(out.Tools))
			continue
		}
		errs = append(errs, matchTool(out.Tools[i], te, i)...)
	}
	return errs
}

func matchMessage(msg translate.OpenAIMessage, exp MessageExpect, i int) []string {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf("messages[%d]: "+format, append([]any{i}, args...)...))
	}

	if exp.Role != nil && msg.Role != *exp.Role {
		add("role = %q, want %q", msg.Role, *exp.Role)
	}
	if exp.Content != nil {
		s, ok := msg.Content.(string)
		if !ok || s != *exp.Content {
			add("content = %v, want %q", msg.Content, *exp.Content)
		}
	}
	if exp.ContentContains != nil {
		s, ok := msg.Content.(string)
		if !ok || !strings.Contains(s, *exp.ContentContains) {
			add("content = %v, want it to contain %q", msg.Content, *exp.ContentContains)
		}
	}
	if exp.ContentNull != nil {
		isNil := msg.Content == nil
		if isNil != *exp.ContentNull {
			add("content = %v, want content_null=%v", msg.Content, *exp.ContentNull)
		}
	}
	if exp.ToolCallsCount != nil && len(msg.ToolCalls) != *exp.ToolCallsCount {
		add("tool_calls count = %d, want %d", len(msg.ToolCalls), *exp.ToolCallsCount)
	}
	if exp.ToolCallNames != nil {
		if len(msg.ToolCalls) != len(exp.ToolCallNames) {
			add("tool_calls count = %d, want %d names %v", len(msg.ToolCalls), len(exp.ToolCallNames), exp.ToolCallNames)
		} else {
			for j, name := range exp.ToolCallNames {
				if msg.ToolCalls[j].Function.Name != name {
					add("tool_calls[%d].function.name = %q, want %q", j, msg.ToolCalls[j].Function.Name, name)
				}
			}
		}
	}
	if exp.ToolCallArgumentsContains != nil {
		if len(msg.ToolCalls) == 0 {
			add("no tool_calls present, want arguments containing %q", *exp.ToolCallArgumentsContains)
		} else if !strings.Contains(msg.ToolCalls[0].Function.Arguments, *exp.ToolCallArgumentsContains) {
			add("tool_calls[0].function.arguments = %q, want it to contain %q", msg.ToolCalls[0].Function.Arguments, *exp.ToolCallArgumentsContains)
		}
	}
	if exp.ToolCallID != nil && msg.ToolCallID != *exp.ToolCallID {
		add("tool_call_id = %q, want %q", msg.ToolCallID, *exp.ToolCallID)
	}
	return errs
}

func matchTool(tool translate.OpenAITool, exp ToolExpect, i int) []string {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf("tools[%d]: "+format, append([]any{i}, args...)...))
	}

	if exp.Name != nil && tool.Function.Name != *exp.Name {
		add("name = %q, want %q", tool.Function.Name, *exp.Name)
	}
	if exp.Description != nil && tool.Function.Description != *exp.Description {
		add("description = %q, want %q", tool.Function.Description, *exp.Description)
	}
	if exp.ParametersObject != nil {
		var m map[string]any
		isObj := json.Unmarshal(tool.Function.Parameters, &m) == nil && m["type"] == "object"
		if isObj != *exp.ParametersObject {
			add("parameters = %s, want parameters_object=%v", tool.Function.Parameters, *exp.ParametersObject)
		}
	}
	if exp.ParametersAbsent != nil {
		absent := len(tool.Function.Parameters) == 0
		if absent != *exp.ParametersAbsent {
			add("parameters = %s, want parameters_absent=%v", tool.Function.Parameters, *exp.ParametersAbsent)
		}
	}
	return errs
}
