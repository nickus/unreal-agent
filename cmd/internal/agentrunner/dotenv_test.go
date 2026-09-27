package agentrunner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/llm"
)

func TestWorkspaceDotEnvDisabled(t *testing.T) {
	for _, test := range []struct {
		name        string
		flag        bool
		environment string
		want        bool
		wantErr     bool
	}{
		{name: "default"},
		{name: "flag", flag: true, want: true},
		{name: "flag wins over false", flag: true, environment: "false", want: true},
		{name: "environment", environment: "1", want: true},
		{name: "environment true", environment: " true ", want: true},
		{name: "environment false", environment: "0"},
		{name: "invalid environment", environment: "sometimes", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := workspaceDotEnvDisabled(test.flag, func(name string) string {
				if name == noWorkspaceDotEnvEnvironment {
					return test.environment
				}
				return ""
			})
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), noWorkspaceDotEnvEnvironment) {
					t.Fatalf("error = %v, want one naming %s", err, noWorkspaceDotEnvEnvironment)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("workspaceDotEnvDisabled(%t, %q) = %t, %v, want %t", test.flag, test.environment, got, err, test.want)
			}
		})
	}
}

// The runner reads its LLM settings through getenv after applying the workspace
// .env, so a workspace can pick the model unless loading is disabled.
func TestRunMainWorkspaceDotEnvOptOut(t *testing.T) {
	unsetEnvironment(t, llmModelEnvironment, noWorkspaceDotEnvEnvironment)
	t.Setenv("OPENAI_API_KEY", "secret")
	for _, test := range []struct {
		name        string
		args        []string
		environment string
		wantModel   string
	}{
		{name: "loaded by default", wantModel: "workspace-model"},
		{name: "flag", args: []string{"-no-workspace-dotenv"}, wantModel: "gpt-default"},
		{name: "environment", environment: "1", wantModel: "gpt-default"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.environment != "" {
				t.Setenv(noWorkspaceDotEnvEnvironment, test.environment)
			}
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, ".env"), []byte(llmModelEnvironment+"=workspace-model\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			models := make(chan string, 1)
			client := &fakeClient{respond: func(_ context.Context, request llm.Request) (llm.Response, error) {
				models <- request.Model.ID
				return llm.Response{ID: "response-1", Stop: llm.StopComplete}, nil
			}}
			var stdout, stderr bytes.Buffer
			args := append([]string{"-workspace", workspace, "-session-directory", t.TempDir()}, test.args...)
			code := RunMain(t.Context(), args, os.Getenv, os.Environ, strings.NewReader(`{"prompt":"hello"}`), &stdout, &stderr, testConfig(client))
			if code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
			}
			if got := <-models; got != test.wantModel {
				t.Fatalf("model = %q, want %q", got, test.wantModel)
			}
			if _, present := os.LookupEnv(llmModelEnvironment); present {
				t.Fatalf("%s leaked out of the run", llmModelEnvironment)
			}
		})
	}
}

func TestRunMainRejectsInvalidWorkspaceDotEnvSwitch(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := RunMain(
		t.Context(),
		[]string{"-workspace", t.TempDir(), "-session-directory", t.TempDir()},
		func(name string) string {
			return map[string]string{"OPENAI_API_KEY": "secret", noWorkspaceDotEnvEnvironment: "maybe"}[name]
		},
		func() []string { return nil },
		strings.NewReader(`{"prompt":"hello"}`),
		&stdout,
		&stderr,
		testConfig(&fakeClient{}),
	)
	if code != 1 || !strings.Contains(stderr.String(), noWorkspaceDotEnvEnvironment) {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
}

func unsetEnvironment(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		previous, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, previous)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}
