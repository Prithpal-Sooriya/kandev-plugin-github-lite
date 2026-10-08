package githublite

import "time"

// Wire types for the small subset of the GitHub REST API this plugin uses.
// Everything is GET except POST /repos/{o}/{r}/pulls (create change request).

type apiUser struct {
	Login   string `json:"login"`
	HTMLURL string `json:"html_url"`
	Avatar  string `json:"avatar_url"`
}

type apiRepository struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	FullName      string  `json:"full_name"`
	Owner         apiUser `json:"owner"`
	Private       bool    `json:"private"`
	HTMLURL       string  `json:"html_url"`
	CloneURL      string  `json:"clone_url"`
	DefaultBranch string  `json:"default_branch"`
}

type apiLabel struct {
	Name string `json:"name"`
}

type apiPullRequest struct {
	Number             int64      `json:"number"`
	State              string     `json:"state"` // "open" | "closed"
	Title              string     `json:"title"`
	HTMLURL            string     `json:"html_url"`
	User               apiUser    `json:"user"`
	Body               string     `json:"body"`
	Draft              bool       `json:"draft"`
	Merged             bool       `json:"merged"`
	Head               apiBranch  `json:"head"`
	Base               apiBranch  `json:"base"`
	MergedAt           *time.Time `json:"merged_at"`
	ClosedAt           *time.Time `json:"closed_at"`
	CreatedAt          *time.Time `json:"created_at"`
	UpdatedAt          *time.Time `json:"updated_at"`
	Mergeable          bool       `json:"mergeable"`
	Additions          int        `json:"additions"`
	Deletions          int        `json:"deletions"`
	RequestedReviewers []apiUser  `json:"requested_reviewers"`
}

type apiBranch struct {
	Ref  string           `json:"ref"`
	Repo apiRepositoryRef `json:"repo"`
	SHA  string           `json:"sha"`
}

// apiRepositoryRef is the slim repository embedded in PR head/base refs.
type apiRepositoryRef struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
}

type apiReview struct {
	ID          int64      `json:"id"`
	User        apiUser    `json:"user"`
	State       string     `json:"state"` // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	Body        string     `json:"body"`
	SubmittedAt *time.Time `json:"submitted_at"`
}

type apiCheckRun struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`     // queued | in_progress | completed
	Conclusion  string     `json:"conclusion"` // success | failure | neutral | cancelled | timed_out | action_required | skipped
	HTMLURL     string     `json:"html_url"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

type apiIssueComment struct {
	ID        int64      `json:"id"`
	User      apiUser    `json:"user"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt *time.Time `json:"created_at"`
}

type apiBranchRef struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}
