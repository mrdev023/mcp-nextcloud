package tools

import "github.com/modelcontextprotocol/go-sdk/mcp"

// effect classifies what a tool does to the user's News data. The MCP tool
// annotations are derived from it, so they cannot drift apart.
type effect int

const (
	readOnly effect = iota
	// reversible changes are undone by the opposite tool (read/unread,
	// star/unstar).
	reversible
	// destructive changes cannot be undone with the tools (mark all as read
	// loses the unread state of every article).
	destructive
)

func (e effect) annotations(title string) *mcp.ToolAnnotations {
	no := false
	a := &mcp.ToolAnnotations{Title: title, OpenWorldHint: &no}
	if e == readOnly {
		a.ReadOnlyHint = true
		return a
	}
	// The spec defaults destructiveHint to true, so write tools always set it.
	isDestructive := e == destructive
	a.DestructiveHint = &isDestructive
	a.IdempotentHint = true
	return a
}

// serverTool is one entry of the tool inventory: its metadata and a function
// that binds its typed handler to a server.
type serverTool struct {
	tool     *mcp.Tool
	effect   effect
	register func(*mcp.Server)
}

func newTool[In, Out any](name, title, description string, e effect, h mcp.ToolHandlerFor[In, Out]) serverTool {
	t := &mcp.Tool{Name: name, Description: description, Annotations: e.annotations(title)}
	return serverTool{tool: t, effect: e, register: func(s *mcp.Server) { mcp.AddTool(s, t, h) }}
}

// inventory lists every tool, bound to api.
func inventory(api NewsAPI) []serverTool {
	t := &handlers{nc: api}
	return []serverTool{
		newTool("news_list_feeds", "List feeds",
			"List the Nextcloud News feeds (id, title, url, folder, unread count) and folders.",
			readOnly, t.listFeeds),
		newTool("news_list_items", "List articles",
			"List Nextcloud News articles, newest first, unread only by default. "+
				"Filter by feedId, folderId or starredOnly; paginate with limit + offset (offset = nextOffset of the previous page). "+
				"Returns short excerpts; use news_get_item for the full article.",
			readOnly, t.listItems),
		newTool("news_get_item", "Get article",
			"Get one Nextcloud News article by id: title, url, author, dates, read/starred state and body (plain text by default).",
			readOnly, t.getItem),
		newTool("news_get_updated", "Get updated articles",
			"Incremental sync: articles created or changed (read/starred state included) since lastModified. "+
				"Expensive on large accounts; prefer news_list_items for browsing.",
			readOnly, t.getUpdated),
		newTool("news_mark_read", "Mark as read",
			"Mark one article (itemId) or several (itemIds) as read.",
			reversible, t.setRead(true)),
		newTool("news_mark_unread", "Mark as unread",
			"Mark one article (itemId) or several (itemIds) as unread.",
			reversible, t.setRead(false)),
		newTool("news_star", "Star article",
			"Star (favourite) an article.",
			reversible, t.setStar(true)),
		newTool("news_unstar", "Unstar article",
			"Remove the star from an article.",
			reversible, t.setStar(false)),
		newTool("news_mark_all_read", "Mark all as read",
			"Mark ALL articles of every feed as read, up to newestItemId (defaults to the newest article). Cannot be undone in bulk.",
			destructive, t.markAllRead),
	}
}

// Options selects the tools Register adds.
type Options struct {
	// ReadOnly leaves out every tool that changes News data, so that the
	// server can only browse articles.
	ReadOnly bool
}

// Register adds the news_* tools selected by opts to s.
func Register(s *mcp.Server, api NewsAPI, opts Options) {
	for _, st := range inventory(api) {
		if opts.ReadOnly && st.effect != readOnly {
			continue
		}
		st.register(s)
	}
}
