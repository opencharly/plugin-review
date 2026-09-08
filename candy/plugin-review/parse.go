package pluginreview

import (
	"encoding/json"
	"os"
	"strings"
)

// ---- small env/parse helpers shared by the engine ----

func getenvAny(names ...string) string {
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok && v != "" {
			return v
		}
	}
	return ""
}

func lookupEnv(k string) (string, bool) { return os.LookupEnv(k) }

// truncateStr truncates s to at most bytes UTF-8 bytes, avoiding a partial
// character at the cut (same behaviour as the action's truncate()).
func truncateStr(s string, bytes int) string {
	if len(s) <= bytes {
		return s
	}
	b := []byte(s)[:bytes]
	// drop a trailing partial rune
	for len(b) > 0 && !utf8Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b) + "\n[…truncated…]"
}

func utf8Valid(b []byte) bool { return json.Valid(append([]byte{'"'}, append(b, '"')...)) }

// ---- GitHub API payload parsers ----

type rawCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Name string `json:"name"`
			Date string `json:"date"`
		} `json:"author"`
		Message string `json:"message"`
	} `json:"commit"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
}

func parseCommits(raw string) ([]prCommit, error) {
	var cs []rawCommit
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		return nil, err
	}
	out := make([]prCommit, 0, len(cs))
	for _, c := range cs {
		author := ""
		if c.Author != nil && c.Author.Login != "" {
			author = c.Author.Login
		} else if c.Commit.Author.Name != "" {
			author = c.Commit.Author.Name
		}
		msg := strings.SplitN(c.Commit.Message, "\n", 2)[0]
		sha := c.SHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		out = append(out, prCommit{SHA: sha, Author: author, Date: c.Commit.Author.Date, Message: msg})
	}
	return out, nil
}

type rawIssue struct {
	Body string `json:"body"`
}
type rawComment struct {
	ID   int `json:"id"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	CreatedAt string `json:"created_at"`
	Body      string `json:"body"`
}

func parseIssueBody(raw string) string {
	var i rawIssue
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		return ""
	}
	return i.Body
}

func parseComments(raw string) []prComment {
	var cs []rawComment
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		return nil
	}
	out := make([]prComment, 0, len(cs))
	for _, c := range cs {
		author := "unknown"
		if c.User.Login != "" {
			author = c.User.Login
		}
		out = append(out, prComment{ID: c.ID, Author: author, CreatedAt: c.CreatedAt, Body: truncateStr(c.Body, 24*1024)})
	}
	return out
}

type rawPull struct {
	Title     string `json:"title"`
	State     string `json:"state"`
	Mergeable *bool  `json:"mergeable"`
	Draft     bool   `json:"draft"`
	Head      struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
}

func parseMeta(raw string) (prMeta, error) {
	var p rawPull
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return prMeta{}, err
	}
	return prMeta{
		Title: p.Title, State: p.State, Mergeable: p.Mergeable, Draft: p.Draft,
		HeadSHA: p.Head.SHA, BaseSHA: p.Base.SHA,
		Additions: p.Additions, Deletions: p.Deletions, ChangedFiles: p.ChangedFiles,
	}, nil
}
