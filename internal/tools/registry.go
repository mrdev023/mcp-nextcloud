// Package tools is the core of the MCP tools: the registry that app modules
// (tools/news, …) declare their tools in, and the error mapping they share.
package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrdev023/mcp-nextcloud/internal/nextcloud"
)

// Effect classifies what a tool does to the user's Nextcloud data. The MCP
// tool annotations are derived from it, so they cannot drift apart.
type Effect int

const (
	ReadOnly Effect = iota
	// Reversible changes are undone by another tool (read/unread,
	// star/unstar, …).
	Reversible
	// Destructive changes cannot be undone with the tools.
	Destructive
)

func (e Effect) annotations(title string) *mcp.ToolAnnotations {
	no := false
	a := &mcp.ToolAnnotations{Title: title, OpenWorldHint: &no}
	if e == ReadOnly {
		a.ReadOnlyHint = true
		return a
	}
	// The spec defaults destructiveHint to true, so write tools always set it.
	isDestructive := e == Destructive
	a.DestructiveHint = &isDestructive
	a.IdempotentHint = true
	return a
}

// ServerTool is one entry of the tool inventory: its metadata and a function
// that binds its typed handler to a server.
type ServerTool struct {
	tool     *mcp.Tool
	effect   Effect
	register func(*mcp.Server)
}

// Effect returns what the tool does to the user's data.
func (t ServerTool) Effect() Effect { return t.effect }

// New declares a tool with a typed handler; its input and output JSON schemas
// are inferred from In and Out.
func New[In, Out any](name, title, description string, e Effect, h mcp.ToolHandlerFor[In, Out]) ServerTool {
	t := &mcp.Tool{Name: name, Description: description, Annotations: e.annotations(title)}
	return ServerTool{tool: t, effect: e, register: func(s *mcp.Server) { mcp.AddTool(s, t, h) }}
}

// Toolset is the group of tools of one Nextcloud app module.
type Toolset struct {
	Name  string // e.g. "news"
	Tools []ServerTool
}

// Options selects the tools Register adds.
type Options struct {
	// ReadOnly leaves out every tool that changes Nextcloud data, so that the
	// server can only browse.
	ReadOnly bool
}

// Register adds the tools of sets selected by opts to s.
func Register(s *mcp.Server, opts Options, sets ...Toolset) {
	for _, set := range sets {
		for _, t := range set.Tools {
			if opts.ReadOnly && t.effect != ReadOnly {
				continue
			}
			t.register(s)
		}
	}
}

// MapErr turns core Nextcloud client errors into clean, user-facing tool
// errors. what names the missing object for ErrNotFound (e.g. "item 12").
func MapErr(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, nextcloud.ErrUnauthorized):
		return errors.New("Nextcloud authentication failed: Nextcloud rejected the credentials (app-password or exchanged token)") //nolint:staticcheck // user-facing, starts with a product name
	case errors.Is(err, nextcloud.ErrNoCredentials):
		return errors.New("missing Nextcloud credentials for this request")
	case errors.Is(err, nextcloud.ErrTokenExchange):
		return fmt.Errorf("could not obtain a Nextcloud token from the identity provider (%w)", err)
	case errors.Is(err, nextcloud.ErrNotFound):
		return fmt.Errorf("%s not found", what)
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("Nextcloud did not answer in time") //nolint:staticcheck // user-facing, starts with a product name
	}
	return err
}
