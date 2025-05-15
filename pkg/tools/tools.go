// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/kubectl-ai/pkg/journal"
	"github.com/google/uuid"
	"sigs.k8s.io/yaml"
)

type ContextKey string

const (
	KubeconfigKey ContextKey = "kubeconfig"
	WorkDirKey    ContextKey = "work_dir"
)

func Lookup(name string) Tool {
	return allTools.Lookup(name)
}

var allTools Tools = Tools{
	tools: make(map[string]Tool),
}

func Default() Tools {
	return allTools
}

// RegisterTool makes a tool available to the LLM.
func RegisterTool(tool Tool) {
	allTools.RegisterTool(tool)
}

type Tools struct {
	tools map[string]Tool
}

func (t *Tools) Lookup(name string) Tool {
	return t.tools[name]
}

func (t *Tools) AllTools() []Tool {
	return slices.Collect(maps.Values(t.tools))
}

func (t *Tools) Names() []string {
	names := make([]string, 0, len(t.tools))
	for name := range t.tools {
		names = append(names, name)
	}
	return names
}

func (t *Tools) RegisterTool(tool Tool) {
	if _, exists := t.tools[tool.Name()]; exists {
		panic("tool already registered: " + tool.Name())
	}
	t.tools[tool.Name()] = tool
}

type ToolCall struct {
	tool      Tool
	name      string
	arguments map[string]any
}

func (t *ToolCall) PrettyPrint() string {
	if command, ok := t.arguments["command"]; ok {
		return command.(string)
	}
	var args []string
	for k, v := range t.arguments {
		args = append(args, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(args)
	return fmt.Sprintf("%s(%s)", t.name, strings.Join(args, ", "))
}

// ParseToolInvocation parses a request from the LLM into a tool call.
func (t *Tools) ParseToolInvocation(ctx context.Context, name string, arguments map[string]any) (*ToolCall, error) {
	tool := t.Lookup(name)
	if tool == nil {
		return nil, fmt.Errorf("tool %q not recognized", name)
	}

	return &ToolCall{
		tool:      tool,
		name:      name,
		arguments: arguments,
	}, nil
}

type InvokeToolOptions struct {
	WorkDir string

	Kubeconfig string
}

type ToolRequestEvent struct {
	CallID    string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type ToolResponseEvent struct {
	CallID   string `json:"id,omitempty"`
	Response any    `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
}

// InvokeTool handles the execution of a single action
func (t *ToolCall) InvokeTool(ctx context.Context, opt InvokeToolOptions) (any, error) {
	recorder := journal.RecorderFromContext(ctx)

	callID := uuid.NewString()
	recorder.Write(ctx, &journal.Event{
		Timestamp: time.Now(),
		Action:    "tool-request",
		Payload: ToolRequestEvent{
			CallID:    callID,
			Name:      t.name,
			Arguments: t.arguments,
		},
	})

	ctx = context.WithValue(ctx, KubeconfigKey, opt.Kubeconfig)
	ctx = context.WithValue(ctx, WorkDirKey, opt.WorkDir)

	response, err := t.tool.Run(ctx, t.arguments)

	{
		ev := ToolResponseEvent{
			CallID:   callID,
			Response: response,
		}
		if err != nil {
			ev.Error = err.Error()
		}
		recorder.Write(ctx, &journal.Event{
			Timestamp: time.Now(),
			Action:    "tool-response",
			Payload:   ev,
		})
	}

	return response, nil
}

// ToolResultToMap converts an arbitrary result to a map[string]any
func ToolResultToMap(result any) (map[string]any, error) {
	b, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("converting result to json: %w", err)
	}

	m := make(map[string]any)
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("converting json result to map: %w", err)
	}
	return m, nil
}

// LoadAndRegisterCustomTools loads tool configurations from a YAML file
// and registers them.
func LoadAndRegisterCustomTools(configPath string) error {
	if configPath == "" {
		// Default config path: ~/.config/kubectl-ai/tools.yaml
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get user home directory for default custom tools config: %w", err)
		}
		configPath = filepath.Join(home, ".config", "kubectl-ai", "tools.yaml")
	}

	yamlFile, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("failed to read config file %s: %w", configPath, err)
	}

	var configs []CustomToolConfig
	err = yaml.Unmarshal(yamlFile, &configs)
	if err != nil {
		return fmt.Errorf("failed to parse YAML config file %s: %w", configPath, err)
	}

	// Register each custom tool
	var registrationErrors []string
	for _, config := range configs {
		tool, err := NewCustomTool(config)
		if err != nil {
			registrationErrors = append(registrationErrors, fmt.Sprintf("failed to create tool %q: %v", config.Name, err))
			continue // Skip registration if creation failed
		}
		// Check for duplicate registration attempt
		if _, exists := allTools.tools[tool.Name()]; exists {
			registrationErrors = append(registrationErrors, fmt.Sprintf("tool %q already registered (possibly built-in), skipping custom definition", tool.Name()))
			continue
		}
		RegisterTool(tool)
	}

	if len(registrationErrors) > 0 {
		return fmt.Errorf("encountered errors during custom tool registration:\n - %s", strings.Join(registrationErrors, "\n - "))
	}

	return nil
}
