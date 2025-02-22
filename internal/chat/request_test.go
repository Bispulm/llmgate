package chat

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const minimal = `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`

func float(f float64) *float64 { return &f }

func TestDecodeRequestAcceptsTheContract(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Request
	}{
		{
			name: "minimal",
			body: minimal,
			want: Request{Model: "fast", Messages: []Message{{RoleUser, "hi"}}},
		},
		{
			name: "every supported field",
			body: `{"model":"fast",
				"messages":[
					{"role":"system","content":"s"},
					{"role":"developer","content":"d"},
					{"role":"user","content":"u"},
					{"role":"assistant","content":"a"},
					{"role":"user","content":"u2"}],
				"max_completion_tokens":5,"temperature":0.5,"top_p":0.25,"stop":["x","y"],
				"stream":true,"stream_options":{"include_usage":true}}`,
			want: Request{
				Model: "fast",
				Messages: []Message{
					{RoleSystem, "s"}, {RoleDeveloper, "d"}, {RoleUser, "u"}, {RoleAssistant, "a"}, {RoleUser, "u2"},
				},
				MaxOutputTokens: 5,
				Temperature:     float(0.5),
				TopP:            float(0.25),
				Stop:            []string{"x", "y"},
				Stream:          true,
				IncludeUsage:    true,
			},
		},
		{
			name: "max_tokens is a synonym",
			body: `{"model":"fast","messages":[{"role":"user","content":"hi"}],"max_tokens":7}`,
			want: Request{Model: "fast", Messages: []Message{{RoleUser, "hi"}}, MaxOutputTokens: 7},
		},
		{
			name: "stop as a string",
			body: `{"model":"fast","messages":[{"role":"user","content":"hi"}],"stop":"end"}`,
			want: Request{Model: "fast", Messages: []Message{{RoleUser, "hi"}}, Stop: []string{"end"}},
		},
		{
			name: "boundary temperature values",
			body: `{"model":"fast","messages":[{"role":"user","content":"hi"}],"temperature":2,"top_p":0}`,
			want: Request{Model: "fast", Messages: []Message{{RoleUser, "hi"}}, Temperature: float(2), TopP: float(0)},
		},
		{
			name: "null is the same as absent for optional fields",
			body: `{"model":"fast","messages":[{"role":"user","content":"hi"}],"max_tokens":null,
				"max_completion_tokens":null,"temperature":null,"top_p":null,"stop":null,"stream":null,
				"stream_options":null,"n":null,"presence_penalty":null,"frequency_penalty":null,"logprobs":null}`,
			want: Request{Model: "fast", Messages: []Message{{RoleUser, "hi"}}},
		},
		{
			name: "explicit defaults have no effect",
			body: `{"model":"fast","messages":[{"role":"user","content":"hi"}],"n":1,"presence_penalty":0,
				"frequency_penalty":0.0,"logprobs":false,"stream":true,"stream_options":{"include_obfuscation":false}}`,
			want: Request{Model: "fast", Messages: []Message{{RoleUser, "hi"}}, Stream: true},
		},
		{
			name: "stream_options before stream",
			body: `{"stream_options":{"include_usage":true},"model":"fast","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			want: Request{Model: "fast", Messages: []Message{{RoleUser, "hi"}}, Stream: true, IncludeUsage: true},
		},
		{
			name: "text is preserved exactly",
			body: `{"model":"fast","messages":[{"role":"user","content":"  héllo\n世界 🙂\t\"q\" é "}]}`,
			want: Request{Model: "fast", Messages: []Message{{RoleUser, "  héllo\n世界 🙂\t\"q\" é "}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeRequest([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*got, tt.want) {
				t.Fatalf("got %+v\nwant %+v", *got, tt.want)
			}
		})
	}
}

func TestDecodeRequestRejectsWithNamedErrors(t *testing.T) {
	msg := func(fields string) string { return `{"model":"fast","messages":[` + fields + `]}` }
	withField := func(field string) string {
		return `{"model":"fast","messages":[{"role":"user","content":"hi"}],` + field + `}`
	}

	tests := []struct {
		name, body  string
		code, param string
	}{
		// Unsupported fields are named, never ignored.
		{"tools", withField(`"tools":[]`), CodeUnsupportedParameter, "tools"},
		{"tools even when null", withField(`"tools":null`), CodeUnsupportedParameter, "tools"},
		{"tool_choice", withField(`"tool_choice":"auto"`), CodeUnsupportedParameter, "tool_choice"},
		{"response_format", withField(`"response_format":{"type":"json_object"}`), CodeUnsupportedParameter, "response_format"},
		{"seed", withField(`"seed":1`), CodeUnsupportedParameter, "seed"},
		{"user", withField(`"user":"u"`), CodeUnsupportedParameter, "user"},
		{"provider specific", withField(`"top_k":5`), CodeUnsupportedParameter, "top_k"},
		{"field names are case sensitive", withField(`"Temperature":1`), CodeUnsupportedParameter, "Temperature"},

		// Explicit defaults are only accepted at the default.
		{"n above one", withField(`"n":2`), CodeUnsupportedParameter, "n"},
		{"presence_penalty", withField(`"presence_penalty":0.5`), CodeUnsupportedParameter, "presence_penalty"},
		{"frequency_penalty", withField(`"frequency_penalty":-1`), CodeUnsupportedParameter, "frequency_penalty"},
		{"logprobs", withField(`"logprobs":true`), CodeUnsupportedParameter, "logprobs"},

		// Missing and wrong types.
		{"no model", `{"messages":[{"role":"user","content":"hi"}]}`, CodeMissingParameter, "model"},
		{"no messages", `{"model":"fast"}`, CodeMissingParameter, "messages"},
		{"null model", `{"model":null,"messages":[{"role":"user","content":"hi"}]}`, CodeInvalidValue, "model"},
		{"empty model", `{"model":"","messages":[{"role":"user","content":"hi"}]}`, CodeInvalidValue, "model"},
		{"long model", `{"model":"` + strings.Repeat("m", 257) + `","messages":[{"role":"user","content":"hi"}]}`, CodeInvalidValue, "model"},
		{"numeric model", `{"model":5,"messages":[{"role":"user","content":"hi"}]}`, CodeInvalidValue, "model"},
		{"empty messages", `{"model":"fast","messages":[]}`, CodeInvalidValue, "messages"},
		{"null messages", `{"model":"fast","messages":null}`, CodeInvalidValue, "messages"},
		{"messages not an array", `{"model":"fast","messages":{}}`, CodeInvalidValue, "messages"},
		{"message not an object", msg(`"hi"`), CodeInvalidValue, "messages[0]"},

		// Output limit.
		{"zero limit", withField(`"max_completion_tokens":0`), CodeInvalidValue, "max_completion_tokens"},
		{"negative limit", withField(`"max_tokens":-1`), CodeInvalidValue, "max_tokens"},
		{"fractional limit", withField(`"max_tokens":1.5`), CodeInvalidValue, "max_tokens"},
		{"string limit", withField(`"max_tokens":"5"`), CodeInvalidValue, "max_tokens"},
		{"both limits", withField(`"max_tokens":1,"max_completion_tokens":1`), CodeInvalidValue, "max_tokens"},

		// Sampling ranges.
		{"temperature high", withField(`"temperature":2.01`), CodeInvalidValue, "temperature"},
		{"temperature low", withField(`"temperature":-0.1`), CodeInvalidValue, "temperature"},
		{"top_p high", withField(`"top_p":1.5`), CodeInvalidValue, "top_p"},
		{"temperature string", withField(`"temperature":"hot"`), CodeInvalidValue, "temperature"},

		// Stop sequences.
		{"too many stops", withField(`"stop":["a","b","c","d","e"]`), CodeInvalidValue, "stop"},
		{"empty stop array", withField(`"stop":[]`), CodeInvalidValue, "stop"},
		{"empty stop string", withField(`"stop":""`), CodeInvalidValue, "stop"},
		{"empty stop element", withField(`"stop":["a",""]`), CodeInvalidValue, "stop"},
		{"non-string stop element", withField(`"stop":[1]`), CodeInvalidValue, "stop"},
		{"numeric stop", withField(`"stop":1`), CodeInvalidValue, "stop"},

		// Stream options.
		{"stream_options without stream", withField(`"stream_options":{"include_usage":true}`), CodeInvalidValue, "stream_options"},
		{"stream_options unknown member", withField(`"stream":true,"stream_options":{"nope":1}`), CodeUnsupportedParameter, "stream_options.nope"},
		{"include_obfuscation true", withField(`"stream":true,"stream_options":{"include_obfuscation":true}`), CodeUnsupportedParameter, "stream_options.include_obfuscation"},
		{"stream_options not an object", withField(`"stream":true,"stream_options":1`), CodeInvalidValue, "stream_options"},
		{"stream not a bool", withField(`"stream":"yes"`), CodeInvalidValue, "stream"},

		// Messages.
		{"tool role", msg(`{"role":"tool","content":"x","tool_call_id":"1"}`), CodeUnsupportedRole, "messages[0].role"},
		{"function role", msg(`{"role":"function","content":"x","name":"f"}`), CodeUnsupportedRole, "messages[0].role"},
		{"unknown role", msg(`{"role":"wizard","content":"x"}`), CodeInvalidValue, "messages[0].role"},
		{"numeric role", msg(`{"role":1,"content":"x"}`), CodeInvalidValue, "messages[0].role"},
		{"missing role", msg(`{"content":"x"}`), CodeMissingParameter, "messages[0].role"},
		{"missing content", msg(`{"role":"user"}`), CodeMissingParameter, "messages[0].content"},
		{"null content", msg(`{"role":"user","content":null}`), CodeInvalidValue, "messages[0].content"},
		{"empty content", msg(`{"role":"user","content":""}`), CodeInvalidValue, "messages[0].content"},
		{"array content", msg(`{"role":"user","content":[{"type":"text","text":"x"}]}`), CodeUnsupportedContentType, "messages[0].content"},
		{"image content", msg(`{"role":"user","content":[{"type":"image_url"}]}`), CodeUnsupportedContentType, "messages[0].content"},
		{"numeric content", msg(`{"role":"user","content":1}`), CodeInvalidValue, "messages[0].content"},
		{"name on a message", msg(`{"role":"user","content":"x","name":"n"}`), CodeUnsupportedParameter, "messages[0].name"},
		{"tool_calls on a message", msg(`{"role":"user","content":"q"},{"role":"assistant","content":"x","tool_calls":[]},{"role":"user","content":"q"}`), CodeUnsupportedParameter, "messages[1].tool_calls"},

		// Ordering.
		{"system after user", msg(`{"role":"user","content":"a"},{"role":"system","content":"b"},{"role":"user","content":"c"}`), CodeInvalidMessageOrder, "messages[1].role"},
		{"ends with assistant", msg(`{"role":"user","content":"a"},{"role":"assistant","content":"b"}`), CodeInvalidMessageOrder, "messages[1].role"},
		{"only system", msg(`{"role":"system","content":"a"}`), CodeInvalidMessageOrder, "messages[0].role"},

		// Ambiguous or broken JSON.
		{"duplicate top-level key", `{"model":"a","model":"b","messages":[{"role":"user","content":"hi"}]}`, CodeDuplicateParameter, "model"},
		{"duplicate message key", msg(`{"role":"user","role":"system","content":"x"}`), CodeDuplicateParameter, "messages[0].role"},
		{"duplicate stream option", withField(`"stream":true,"stream_options":{"include_usage":true,"include_usage":false}`), CodeDuplicateParameter, "stream_options.include_usage"},
		{"empty body", ``, CodeInvalidJSON, ""},
		{"not an object", `[1]`, CodeInvalidJSON, ""},
		{"truncated", `{"model":"fast"`, CodeInvalidJSON, ""},
		{"trailing comma", `{"model":"fast",}`, CodeInvalidJSON, ""},
		{"trailing data", minimal + `{}`, CodeInvalidJSON, ""},
		{"invalid UTF-8 in text", "{\"model\":\"fast\",\"messages\":[{\"role\":\"user\",\"content\":\"\xff\"}]}", CodeInvalidJSON, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeRequest([]byte(tt.body))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("error = %v, want a ValidationError", err)
			}
			if ve.Code != tt.code || ve.Param != tt.param {
				t.Fatalf("got code %q param %q (%s), want code %q param %q", ve.Code, ve.Param, ve.Message, tt.code, tt.param)
			}
			if ve.Message == "" {
				t.Fatal("error has no message")
			}
		})
	}
}

// A field name comes from the client, so one in an error is bounded.
func TestDecodeRequestBoundsEchoedFieldNames(t *testing.T) {
	long := strings.Repeat("k", 10000)
	_, err := DecodeRequest([]byte(`{"` + long + `":1}`))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v", err)
	}
	if len(ve.Param) > 128 || len(ve.Message) > 512 {
		t.Fatalf("param %d bytes, message %d bytes", len(ve.Param), len(ve.Message))
	}
}

// Validation errors reach clients and logs, so they must not repeat message text.
func TestDecodeRequestErrorsDoNotRepeatContent(t *testing.T) {
	const secret = "prompt-text-must-not-appear"
	bodies := []string{
		`{"model":"fast","messages":[{"role":"` + secret + `","content":"x"}]}`,
		`{"model":"fast","messages":[{"role":"tool","content":"` + secret + `"}]}`,
		`{"model":"fast","messages":[{"role":"assistant","content":"` + secret + `"}]}`,
		`{"model":"` + secret + `","messages":[{"role":"user","content":""}]}`,
		`{"model":"fast","messages":[{"role":"user","content":"` + secret + `"}],"stop":[""]}`,
	}
	for _, body := range bodies {
		_, err := DecodeRequest([]byte(body))
		if err == nil {
			t.Fatalf("accepted %s", body)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error repeats content: %v", err)
		}
	}
}

func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(minimal))
	f.Add([]byte(`{"model":"a","messages":[{"role":"user","content":"x"}],"stop":["a"],"stream":true,"stream_options":{"include_usage":true}}`))
	f.Add([]byte(`{"model":"a","model":"b"}`))
	f.Add([]byte("{\"\xff\":1}"))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := DecodeRequest(body)
		if err == nil {
			if req.Model == "" || len(req.Messages) == 0 {
				t.Fatalf("accepted an incomplete request: %+v", req)
			}
			if req.Messages[len(req.Messages)-1].Role != RoleUser {
				t.Fatalf("accepted a request not ending with a user message: %+v", req)
			}
			return
		}
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("non-validation error: %v", err)
		}
	})
}
