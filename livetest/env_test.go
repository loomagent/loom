package livetest

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestLoadLiveBlocks covers the configuration the suite runs on: one provider may name several
// models, and a mistake in the file is reported rather than quietly shrinking the run.
func TestLoadLiveBlocks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		file    string
		want    []liveBlock
		wantErr string // a substring the error must mention
	}{
		{
			name: "one provider runs several models in the order they are listed",
			file: `
LOOM_LIVE_PROVIDERS=deepseek
LOOM_LIVE_DEEPSEEK_URL=https://api.deepseek.com/v1
LOOM_LIVE_DEEPSEEK_KEY=secret
LOOM_LIVE_DEEPSEEK_MODELS=model-a, model-b
`,
			want: []liveBlock{{
				Provider: "deepseek",
				BaseURL:  "https://api.deepseek.com/v1",
				APIKey:   "secret",
				Effort:   "low",
				Models:   []string{"model-a", "model-b"},
			}},
		},
		{
			name: "listed providers are returned in order, and an unlisted block is not",
			file: `
LOOM_LIVE_PROVIDERS=zhipuai,ark
LOOM_LIVE_DEEPSEEK_URL=https://api.deepseek.com/v1
LOOM_LIVE_DEEPSEEK_KEY=secret
LOOM_LIVE_DEEPSEEK_MODELS=model-a
LOOM_LIVE_ZHIPUAI_URL=https://open.bigmodel.cn/api/paas/v4
LOOM_LIVE_ZHIPUAI_KEY=secret
LOOM_LIVE_ZHIPUAI_MODELS=model-c
LOOM_LIVE_ARK_URL=https://ark.cn-beijing.volces.com/api/v3
LOOM_LIVE_ARK_KEY=secret
LOOM_LIVE_ARK_MODELS=model-d
`,
			want: []liveBlock{
				{Provider: "zhipuai", BaseURL: "https://open.bigmodel.cn/api/paas/v4", APIKey: "secret", Effort: "low", Models: []string{"model-c"}},
				{Provider: "ark", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", APIKey: "secret", Effort: "low", Models: []string{"model-d"}},
			},
		},
		{
			name: "a declared effort overrides the default, and an empty one clears it",
			file: `
LOOM_LIVE_PROVIDERS=deepseek,ark
LOOM_LIVE_DEEPSEEK_URL=https://api.deepseek.com/v1
LOOM_LIVE_DEEPSEEK_KEY=secret
LOOM_LIVE_DEEPSEEK_MODELS=model-a
LOOM_LIVE_DEEPSEEK_EFFORT=xhigh
LOOM_LIVE_ARK_URL=https://ark.cn-beijing.volces.com/api/v3
LOOM_LIVE_ARK_KEY=secret
LOOM_LIVE_ARK_MODELS=model-d
LOOM_LIVE_ARK_EFFORT=
`,
			want: []liveBlock{
				{Provider: "deepseek", BaseURL: "https://api.deepseek.com/v1", APIKey: "secret", Effort: "xhigh", Models: []string{"model-a"}},
				{Provider: "ark", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", APIKey: "secret", Effort: "", Models: []string{"model-d"}},
			},
		},
		{
			name: "comments, blank lines, quoted values, and '=' inside a value survive parsing",
			file: `
# which providers to run

LOOM_LIVE_PROVIDERS=deepseek
LOOM_LIVE_DEEPSEEK_URL="https://api.deepseek.com/v1"
LOOM_LIVE_DEEPSEEK_KEY='sk=a=b'
LOOM_LIVE_DEEPSEEK_MODELS=model-a,
`,
			want: []liveBlock{{
				Provider: "deepseek",
				BaseURL:  "https://api.deepseek.com/v1",
				APIKey:   "sk=a=b",
				Effort:   "low",
				Models:   []string{"model-a"},
			}},
		},
		{
			name:    "a block that names no provider to run is an error",
			file:    "LOOM_LIVE_DEEPSEEK_MODELS=model-a\n",
			wantErr: "LOOM_LIVE_PROVIDERS names no provider",
		},
		{
			name: "a provider name that is not a provider is an error",
			file: `
LOOM_LIVE_PROVIDERS=deepsek
LOOM_LIVE_DEEPSEK_URL=https://api.deepseek.com/v1
LOOM_LIVE_DEEPSEK_KEY=secret
LOOM_LIVE_DEEPSEK_MODELS=model-a
`,
			wantErr: "is not a provider",
		},
		{
			name: "a missing key is an error",
			file: `
LOOM_LIVE_PROVIDERS=deepseek
LOOM_LIVE_DEEPSEEK_URL=https://api.deepseek.com/v1
LOOM_LIVE_DEEPSEEK_MODELS=model-a
`,
			wantErr: "LOOM_LIVE_DEEPSEEK_KEY is empty",
		},
		{
			name: "a missing endpoint is an error",
			file: `
LOOM_LIVE_PROVIDERS=deepseek
LOOM_LIVE_DEEPSEEK_KEY=secret
LOOM_LIVE_DEEPSEEK_MODELS=model-a
`,
			wantErr: "LOOM_LIVE_DEEPSEEK_URL is empty",
		},
		{
			name: "an endpoint that is not http is an error",
			file: `
LOOM_LIVE_PROVIDERS=deepseek
LOOM_LIVE_DEEPSEEK_URL=api.deepseek.com
LOOM_LIVE_DEEPSEEK_KEY=secret
LOOM_LIVE_DEEPSEEK_MODELS=model-a
`,
			wantErr: "want an http(s) endpoint",
		},
		{
			name: "a block with no model is an error",
			file: `
LOOM_LIVE_PROVIDERS=deepseek
LOOM_LIVE_DEEPSEEK_URL=https://api.deepseek.com/v1
LOOM_LIVE_DEEPSEEK_KEY=secret
LOOM_LIVE_DEEPSEEK_MODELS=,
`,
			wantErr: "LOOM_LIVE_DEEPSEEK_MODELS names no model",
		},
		{
			name: "an effort that is not a token is an error",
			file: `
LOOM_LIVE_PROVIDERS=deepseek
LOOM_LIVE_DEEPSEEK_URL=https://api.deepseek.com/v1
LOOM_LIVE_DEEPSEEK_KEY=secret
LOOM_LIVE_DEEPSEEK_MODELS=model-a
LOOM_LIVE_DEEPSEEK_EFFORT="extra high"
`,
			wantErr: "LOOM_LIVE_DEEPSEEK_EFFORT",
		},
		{
			name: "a line that is not KEY=VALUE is an error that names its line",
			file: `
LOOM_LIVE_PROVIDERS=deepseek
oops
`,
			wantErr: "env:3: expected KEY=VALUE",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "live.env")
			writeFile(t, path, test.file)
			got, err := loadLiveBlocks(path)
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("loadLiveBlocks succeeded with %+v, want an error mentioning %q", got, test.wantErr)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("loadLiveBlocks error = %v, want it to mention %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadLiveBlocks: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("loadLiveBlocks returned %+v, want %+v", got, test.want)
			}
		})
	}
}

// writeFile writes body to path, failing the test if it cannot.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestLiveModelListsAreNotFilledIn enforces a rule rather than a behaviour: the example file and
// the documentation describe the shape of the configuration and never fill it in. A model name
// in either place becomes a default somebody runs and pays for, and which models a credential
// may call is the operator's decision, not the repository's.
func TestLiveModelListsAreNotFilledIn(t *testing.T) {
	t.Parallel()
	t.Run("the example leaves the models empty", func(t *testing.T) {
		t.Parallel()
		declared := 0
		for _, line := range assignmentLines(t, "../live.env.example") {
			if !strings.HasSuffix(line.key, "_MODELS") {
				continue
			}
			declared++
			if line.value != "" {
				t.Errorf("../live.env.example:%d: %s = %q names a model; the example shows the shape of the configuration and leaves the choice to the operator",
					line.number, line.key, line.value)
			}
		}
		if declared == 0 {
			t.Errorf("../live.env.example declares no *_MODELS, so it does not show the shape of the configuration")
		}
	})
	t.Run("the documentation shows a placeholder", func(t *testing.T) {
		t.Parallel()
		for _, path := range []string{"../README.md", "../README.zh-CN.md"} {
			for _, line := range assignmentLines(t, path) {
				if !strings.HasSuffix(line.key, "_MODELS") {
					continue
				}
				if !strings.Contains(line.value, "<") || !strings.Contains(line.value, ">") {
					t.Errorf("%s:%d: %s = %q shows a model; documentation shows the shape of the configuration with a placeholder",
						path, line.number, line.key, line.value)
				}
			}
		}
	})
}

// assignment is one KEY=VALUE line of a documentation or example file.
type assignment struct {
	number int
	key    string
	value  string
}

// assignmentLines returns the KEY=VALUE lines of path, skipping blanks and comments. It reads the
// file as text rather than as configuration, because what matters is what a reader copies.
func assignmentLines(t *testing.T, path string) []assignment {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := make([]assignment, 0, 16)
	for index, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		lines = append(lines, assignment{number: index + 1, key: strings.TrimSpace(key), value: strings.Trim(strings.TrimSpace(value), `"'`)})
	}
	return lines
}
