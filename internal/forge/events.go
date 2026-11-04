package forge

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/gitserver"
)

// The kinds of forge.
const (
	GitHub  = "github"
	GitLab  = "gitlab"
	Gitea   = "gitea"
	Generic = "generic"
)

// The kinds of event.
const (
	Ignored     = ""
	Push        = "push"
	PullChanged = "pull"
	PullClosed  = "pull-closed"
)

// Event is what a forge has told of.
type Event struct {
	Kind string
	// Branch and Commit are what a push moved. No commit says that the branch has gone. The commit is only a hint:
	// what gets deployed is what is fetched from the repository, and not what anyone says is there.
	Branch, Commit string
	// DefaultBranch is the repository's, if it said.
	DefaultBranch string
	// For a pull request.
	Number int
	Title  string
	Head   string
	// Author is who pushed, or whose pull request it is.
	Author string
	// FromFork says that its code is not in this repository, and Member that its author may push there, as far as the
	// forge says. A forge that does not say has it from the repository it is in.
	FromFork, Member bool
}

// Verified says whether a request came from whoever has the hook's secret.
func Verified(kind, secret string, header http.Header, body []byte) bool {
	expected := func(prefix string) string {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		return prefix + hex.EncodeToString(mac.Sum(nil))
	}
	same := func(a, b string) bool { return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
	switch kind {
	case GitHub:
		return same(header.Get("X-Hub-Signature-256"), expected("sha256="))
	case Gitea:
		return same(header.Get("X-Gitea-Signature"), expected(""))
	case GitLab:
		return same(header.Get("X-Gitlab-Token"), secret) // it sends the secret itself
	}
	return same(header.Get("X-Hub-Signature-256"), expected("sha256=")) || same(header.Get("X-Moorage-Signature"), expected(""))
}

type hookPayload struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
	Commit  string `json:"commit"`
	Sender  struct {
		Login string `json:"login"`
	} `json:"sender"`
	UserUsername string `json:"user_username"`
	Action       string `json:"action"`
	Number       int    `json:"number"`
	Repository   struct {
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Project struct {
		DefaultBranch string `json:"default_branch"`
	} `json:"project"`
	PullRequest struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Merged bool   `json:"merged"`
		State  string `json:"state"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		AuthorAssociation string `json:"author_association"`
		Head              struct {
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
	// GitLab's way of saying most of this.
	User struct {
		Username string `json:"username"`
	} `json:"user"`
	ObjectAttributes struct {
		IID             int    `json:"iid"`
		Title           string `json:"title"`
		Action          string `json:"action"`
		State           string `json:"state"`
		SourceProjectID int    `json:"source_project_id"`
		TargetProjectID int    `json:"target_project_id"`
		LastCommit      struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
}

// ParseEvent reads what a forge sent. An event of a kind that is of no interest here is no error: forges send many.
func ParseEvent(kind string, header http.Header, body []byte) (Event, error) {
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, errors.New("what the forge sent is not JSON: " + err.Error())
	}
	e := Event{DefaultBranch: p.Repository.DefaultBranch}
	switch kind {
	case GitHub, Gitea:
		name := header.Get("X-GitHub-Event")
		if kind == Gitea {
			name = header.Get("X-Gitea-Event")
		}
		switch name {
		case "push":
			parsePush(&e, p.Ref, p.After, p.Deleted)
			e.Author = p.Sender.Login
		case "pull_request":
			pr := p.PullRequest
			number := p.Number
			if number == 0 {
				number = pr.Number
			}
			e.Number, e.Title, e.Author, e.Head = number, pr.Title, pr.User.Login, pr.Head.SHA
			// A pull request whose head has gone, or that does not say, is not vouched for by anyone.
			e.FromFork = pr.Head.Repo.FullName == "" || pr.Head.Repo.FullName != pr.Base.Repo.FullName
			if kind == GitHub {
				switch pr.AuthorAssociation {
				case "OWNER", "MEMBER", "COLLABORATOR":
					e.Member = true
				}
			} else {
				e.Member = !e.FromFork
			}
			switch p.Action {
			case "opened", "reopened", "synchronize", "synchronized":
				e.Kind = PullChanged
			case "closed":
				e.Kind = PullClosed
			}
		}
	case GitLab:
		e.DefaultBranch = p.Project.DefaultBranch
		switch header.Get("X-Gitlab-Event") {
		case "Push Hook":
			parsePush(&e, p.Ref, p.After, false)
			e.Author = p.UserUsername
		case "Merge Request Hook":
			mr := p.ObjectAttributes
			e.Number, e.Title, e.Author, e.Head = mr.IID, mr.Title, p.User.Username, mr.LastCommit.ID
			e.FromFork = mr.SourceProjectID == 0 || mr.SourceProjectID != mr.TargetProjectID
			e.Member = !e.FromFork
			switch mr.Action {
			case "open", "reopen", "update":
				e.Kind = PullChanged
			case "close", "merge":
				e.Kind = PullClosed
			}
		}
	default:
		commit := p.After
		if commit == "" {
			commit = p.Commit
		}
		parsePush(&e, p.Ref, commit, p.Deleted)
	}
	return e, nil
}

func parsePush(e *Event, ref, commit string, deleted bool) {
	branch, isBranch := strings.CutPrefix(ref, "refs/heads/")
	if !isBranch || branch == "" {
		return // a tag: kept, and not deployed, as with a push here
	}
	if !gitserver.ValidRef(ref) {
		return
	}
	e.Kind, e.Branch, e.Commit = Push, branch, commit
	if deleted || strings.Trim(commit, "0") == "" {
		e.Commit = ""
	}
}
