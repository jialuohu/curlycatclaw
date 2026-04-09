package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jialuohu/curlycatclaw/internal/update"
)

// NewManageServiceSkill creates the manage_service skill for companion Docker service management.
func NewManageServiceSkill(client *update.Client) *Skill {
	return &Skill{
		Name:        "manage_service",
		Description: "Manage companion Docker services (e.g. MCP servers). Register new services dynamically, start/stop them, check status. Services run as Docker containers managed by the updater sidecar.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action":      {"type": "string", "enum": ["register", "remove", "start", "stop", "status", "list"], "description": "Action to perform"},
				"name":        {"type": "string", "description": "Service name (required for all except list)"},
				"image":       {"type": "string", "description": "Docker image (required for register, e.g. xpzouying/xiaohongshu-mcp)"},
				"ports":       {"type": "object", "additionalProperties": {"type": "string"}, "description": "Host:container port map (for register)"},
				"volumes":     {"type": "object", "additionalProperties": {"type": "string"}, "description": "Volume-name:container-path map (for register, named volumes only)"},
				"env":         {"type": "object", "additionalProperties": {"type": "string"}, "description": "Environment variables (for register)"},
				"healthcheck": {"type": "object", "properties": {"test": {"type": "array", "items": {"type": "string"}}, "interval": {"type": "string"}, "timeout": {"type": "string"}, "retries": {"type": "integer"}}, "description": "Docker healthcheck config (for register)"}
			},
			"required": ["action"]
		}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			if client == nil {
				return "", fmt.Errorf("service management requires the updater sidecar. Enable it with COMPOSE_PROFILES=updater in your .env file")
			}

			var params struct {
				Action      string                     `json:"action"`
				Name        string                     `json:"name"`
				Image       string                     `json:"image"`
				Ports       map[string]string          `json:"ports"`
				Volumes     map[string]string          `json:"volumes"`
				Env         map[string]string          `json:"env"`
				Healthcheck *update.ServiceHealthcheck `json:"healthcheck"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}

			switch params.Action {
			case "register":
				if params.Name == "" || params.Image == "" {
					return "", fmt.Errorf("name and image are required for register")
				}
				spec := update.ServiceSpec{
					Name:        params.Name,
					Image:       params.Image,
					Ports:       params.Ports,
					Volumes:     params.Volumes,
					Env:         params.Env,
					Healthcheck: params.Healthcheck,
				}
				if err := client.ServiceRegister(ctx, spec); err != nil {
					return "", fmt.Errorf("register service: %w", err)
				}
				return fmt.Sprintf("Service %q registered (image: %s). Use action \"start\" to start it.", params.Name, params.Image), nil

			case "remove":
				if params.Name == "" {
					return "", fmt.Errorf("name is required for remove")
				}
				if err := client.ServiceRemove(ctx, params.Name); err != nil {
					return "", fmt.Errorf("remove service: %w", err)
				}
				return fmt.Sprintf("Service %q removed.", params.Name), nil

			case "start":
				if params.Name == "" {
					return "", fmt.Errorf("name is required for start")
				}
				if err := client.ServiceStart(ctx, params.Name); err != nil {
					return "", fmt.Errorf("start service: %w", err)
				}
				return fmt.Sprintf("Service %q is starting. Poll with action \"status\" to check readiness.", params.Name), nil

			case "stop":
				if params.Name == "" {
					return "", fmt.Errorf("name is required for stop")
				}
				if err := client.ServiceStop(ctx, params.Name); err != nil {
					return "", fmt.Errorf("stop service: %w", err)
				}
				return fmt.Sprintf("Service %q stopped.", params.Name), nil

			case "status":
				if params.Name == "" {
					return "", fmt.Errorf("name is required for status")
				}
				st, err := client.ServiceStatusCheck(ctx, params.Name)
				if err != nil {
					return "", fmt.Errorf("service status: %w", err)
				}
				health := st.Health
				if health == "" {
					health = "no healthcheck"
				}
				return fmt.Sprintf("Service %q: status=%s, health=%s", st.Name, st.Status, health), nil

			case "list":
				services, err := client.ServiceList(ctx)
				if err != nil {
					return "", fmt.Errorf("list services: %w", err)
				}
				if len(services) == 0 {
					return "No managed services registered.", nil
				}
				var sb strings.Builder
				sb.WriteString("Managed services:\n\n")
				for _, s := range services {
					health := s.Health
					if health == "" {
						health = "no healthcheck"
					}
					fmt.Fprintf(&sb, "- **%s**: %s (health: %s)\n", s.Name, s.Status, health)
				}
				return sb.String(), nil

			default:
				return "", fmt.Errorf("unknown action %q, must be register/remove/start/stop/status/list", params.Action)
			}
		},
	}
}
