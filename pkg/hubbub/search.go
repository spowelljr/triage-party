package hubbub

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/hokaccha/go-prettyjson"

	"github.com/google/go-github/v31/github"
	"github.com/google/triage-party/pkg/logu"
	"github.com/google/triage-party/pkg/tag"
	"k8s.io/klog/v2"
)

// Search for GitHub issues or PR's
func (h *Engine) SearchAny(ctx context.Context, org string, project string, fs []Filter, newerThan time.Time, hidden bool) ([]*Conversation, time.Time, error) {
	cs, ts, err := h.SearchIssues(ctx, org, project, fs, newerThan, hidden)
	if err != nil {
		return cs, ts, err
	}

	pcs, pts, err := h.SearchPullRequests(ctx, org, project, fs, newerThan, hidden)
	if err != nil {
		return cs, ts, err
	}

	if pts.After(ts) {
		ts = pts
	}

	return append(cs, pcs...), ts, nil
}

// Search for GitHub issues or PR's
func (h *Engine) SearchIssues(ctx context.Context, org string, project string, fs []Filter, newerThan time.Time, hidden bool) ([]*Conversation, time.Time, error) {
	fs = openByDefault(fs)
	klog.V(1).Infof("Gathering raw data for %s/%s search %s - newer than %s", org, project, toYAML(fs), logu.STime(newerThan))
	var wg sync.WaitGroup

	var open []*github.Issue
	var closed []*github.Issue
	var err error

	age := time.Now()

	wg.Add(1)
	go func() {
		defer wg.Done()
		oi, ots, err := h.cachedIssues(ctx, org, project, "open", 0, newerThan)
		if err != nil {
			klog.Errorf("open issues: %v", err)
			return
		}
		if ots.Before(age) {
			age = ots
		}
		open = oi
		klog.V(1).Infof("%s/%s open issue count: %d", org, project, len(open))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if !NeedsClosed(fs) {
			return
		}

		ci, cts, err := h.cachedIssues(ctx, org, project, "closed", h.MaxClosedUpdateAge, newerThan)
		if err != nil {
			klog.Errorf("closed issues: %v", err)
		}

		if cts.Before(age) {
			age = cts
		}
		closed = ci

		klog.V(1).Infof("%s/%s closed issue count: %d", org, project, len(closed))
	}()

	wg.Wait()

	var is []*github.Issue
	seen := map[string]bool{}

	for _, i := range append(open, closed...) {
		if h.debugNumber != 0 {
			if i.GetNumber() == h.debugNumber {
				klog.Errorf("*** Found debug issue #%d:\n%s", i.GetNumber(), formatStruct(*i))
			} else {
				continue
			}
		}

		if seen[i.GetURL()] {
			klog.Errorf("unusual: I already saw #%d", i.GetNumber())
			continue
		}
		seen[i.GetURL()] = true
		is = append(is, i)
	}

	var filtered []*Conversation
	klog.V(1).Infof("%s/%s aggregate issue count: %d, filtering for:\n%s", org, project, len(is), toYAML(fs))

	// Avoids updating PR references on a quiet repository
	mostRecentUpdate := time.Time{}
	for _, i := range is {
		if i.GetUpdatedAt().After(mostRecentUpdate) {
			mostRecentUpdate = i.GetUpdatedAt()
		}
	}

	for _, i := range is {
		// Inconsistency warning: issues use a list of labels, prs a list of label pointers
		labels := []*github.Label{}
		for _, l := range i.Labels {
			l := l
			labels = append(labels, l)
		}

		if !preFetchMatch(i, labels, fs) {
			klog.V(1).Infof("#%d - %q did not match item filter: %s", i.GetNumber(), i.GetTitle(), toYAML(fs))
			continue
		}

		klog.V(1).Infof("#%d - %q made it past pre-fetch: %s", i.GetNumber(), i.GetTitle(), toYAML(fs))

		comments := []*github.IssueComment{}

		if needComments(i, fs, newerThan, hidden) && i.GetComments() > 0 {
			klog.V(1).Infof("#%d - %q: need comments for final filtering", i.GetNumber(), i.GetTitle())
			comments, _, err = h.cachedIssueComments(ctx, org, project, i.GetNumber(), h.mtime(i))
			if err != nil {
				klog.Errorf("comments: %v", err)
			}
		}

		co := h.IssueSummary(i, comments, age)
		co.Labels = labels
		h.seen[co.URL] = co

		co.Similar = h.FindSimilar(co)
		if len(co.Similar) > 0 {
			co.Tags = append(co.Tags, tag.Similar)
		}

		if !postFetchMatch(co, fs) {
			klog.V(1).Infof("#%d - %q did not match post-fetch filter: %s", i.GetNumber(), i.GetTitle(), toYAML(fs))
			continue
		}
		klog.V(1).Infof("#%d - %q made it past post-fetch: %s", i.GetNumber(), i.GetTitle(), toYAML(fs))

		updatedAt := h.mtime(i)
		var timeline []*github.Timeline
		if needTimeline(i, fs, newerThan, hidden) {
			timeline, err = h.cachedTimeline(ctx, org, project, i.GetNumber(), updatedAt)
			if err != nil {
				klog.Errorf("timeline: %v", err)
				continue
			}
		}

		h.addEvents(ctx, co, timeline)

		// Some labels are judged by linked PR state. Ensure that they are updated to the same timestamp.
		if needReviews(i, fs, newerThan, hidden) && len(co.PullRequestRefs) > 0 {
			co.PullRequestRefs = h.updateLinkedPRs(ctx, co, mostRecentUpdate)
		}

		if !postEventsMatch(co, fs) {
			klog.V(1).Infof("#%d - %q did not match post-events filter: %s", i.GetNumber(), i.GetTitle(), toYAML(fs))
			continue
		}
		klog.V(1).Infof("#%d - %q made it past post-events: %s", i.GetNumber(), i.GetTitle(), toYAML(fs))

		filtered = append(filtered, co)
	}

	klog.V(1).Infof("%d of %d issues within %s/%s matched filters %s", len(filtered), len(is), org, project, toYAML(fs))
	return filtered, age, nil
}

// NeedsClosed returns whether or not the filters require closed items
func NeedsClosed(fs []Filter) bool {
	// First-pass filter: do any filters require closed data?
	for _, f := range fs {
		if f.ClosedCommenters != "" {
			klog.Infof("will need closed items due to ClosedCommenters=%s", f.ClosedCommenters)
			return true
		}
		if f.ClosedComments != "" {
			klog.Infof("will need closed items due to ClosedComments=%s", f.ClosedComments)
			return true
		}
		if f.State != "" && f.State != "open" {
			klog.Infof("will need closed items due to State=%s", f.State)
			return true
		}
	}
	return false
}

func (h *Engine) SearchPullRequests(ctx context.Context, org string, project string, fs []Filter, newerThan time.Time, hidden bool) ([]*Conversation, time.Time, error) {
	fs = openByDefault(fs)

	klog.V(1).Infof("Searching %s/%s for PR's matching: %s - newer than %s", org, project, toYAML(fs), logu.STime(newerThan))
	filtered := []*Conversation{}

	var wg sync.WaitGroup

	var open []*github.PullRequest
	var closed []*github.PullRequest
	var err error
	age := time.Now()

	wg.Add(1)
	go func() {
		defer wg.Done()
		op, ots, err := h.cachedPRs(ctx, org, project, "open", 0, newerThan)
		if err != nil {
			klog.Errorf("open prs: %v", err)
			return
		}
		if ots.Before(age) {
			age = ots
		}
		open = op
		klog.V(1).Infof("open PR count: %d", len(open))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		if !NeedsClosed(fs) {
			return
		}
		cp, cts, err := h.cachedPRs(ctx, org, project, "closed", h.MaxClosedUpdateAge, newerThan)
		if err != nil {
			klog.Errorf("closed prs: %v", err)
			return
		}

		if cts.Before(age) {
			age = cts
		}
		closed = cp

		klog.V(1).Infof("closed PR count: %d", len(closed))
	}()

	wg.Wait()

	var latest time.Time
	prs := []*github.PullRequest{}
	for _, pr := range append(open, closed...) {
		if pr.GetUpdatedAt().After(latest) {
			latest = pr.GetUpdatedAt()
		}

		if h.debugNumber != 0 {
			if pr.GetNumber() == h.debugNumber {
				klog.Errorf("*** Found debug PR #%d:\n%s", pr.GetNumber(), formatStruct(*pr))
			} else {
				continue
			}
		}
		prs = append(prs, pr)
	}

	for _, pr := range prs {
		klog.V(3).Infof("Found PR #%d with labels: %+v", pr.GetNumber(), pr.Labels)
		if !preFetchMatch(pr, pr.Labels, fs) {
			klog.V(4).Infof("PR #%d did not pass preFetchMatch :(", pr.GetNumber())
			continue
		}

		var timeline []*github.Timeline
		var reviews []*github.PullRequestReview
		var comments []*Comment

		// pr.GetComments() always returns 0 :(
		if needComments(pr, fs, newerThan, hidden) {
			comments, _, err = h.prComments(ctx, org, project, pr.GetNumber(), h.mtime(pr))
			if err != nil {
				klog.Errorf("comments: %v", err)
			}
		}

		if needTimeline(pr, fs, newerThan, hidden) {
			timeline, err = h.cachedTimeline(ctx, org, project, pr.GetNumber(), h.mtime(pr))
			if err != nil {
				klog.Errorf("timeline: %v", err)
				continue
			}
		}

		if needReviews(pr, fs, newerThan, hidden) {
			reviews, _, err = h.cachedReviews(ctx, org, project, pr.GetNumber(), h.mtime(pr))
			if err != nil {
				klog.Errorf("reviews: %v", err)
				continue
			}
		}

		if pr.GetNumber() == h.debugNumber {
			klog.Errorf("*** Debug PR timeline #%d:\n%s", pr.GetNumber(), formatStruct(timeline))
		}

		co := h.PRSummary(ctx, pr, comments, timeline, reviews, age)
		co.Labels = pr.Labels
		co.Similar = h.FindSimilar(co)
		if len(co.Similar) > 0 {
			co.Tags = append(co.Tags, tag.Similar)
		}

		h.seen[co.URL] = co
		if !postFetchMatch(co, fs) {
			klog.V(4).Infof("PR #%d did not pass postFetchMatch with filter: %v", pr.GetNumber(), fs)
			continue
		}

		if !postEventsMatch(co, fs) {
			klog.V(1).Infof("#%d - %q did not match post-events filter: %s", pr.GetNumber(), pr.GetTitle(), toYAML(fs))
			continue
		}

		filtered = append(filtered, co)
	}

	klog.V(1).Infof("%d of %d PR's within %s/%s matched filters:\n%s", len(filtered), len(prs), org, project, toYAML(fs))
	return filtered, latest, nil
}

func needComments(i GitHubItem, fs []Filter, newerThan time.Time, hidden bool) bool {
	if newerThan.IsZero() {
		return false
	}

	for _, f := range fs {
		if f.TagRegex() != nil {
			if ok, t := matchTag(tag.Tags, f.TagRegex(), f.TagNegate()); ok {
				if t.NeedsComments {
					klog.V(1).Infof("#%d - need comments due to tag %s (negate=%v)", i.GetNumber(), f.TagRegex(), f.TagNegate())
					return true
				}
			}
		}

		if f.ClosedCommenters != "" || f.ClosedComments != "" {
			klog.V(1).Infof("#%d - need comments due to closed comments", i.GetNumber())
			return true
		}

		if f.Responded != "" || f.Commenters != "" {
			klog.V(1).Infof("#%d - need comments due to responded/commenters filter", i.GetNumber())
			return true
		}
	}

	if newerThan.IsZero() {
		return false
	}

	if i.GetState() != "open" {
		return false
	}

	// Do we need it? Not really. But it's useful for users to see the tags
	return true
}

func needTimeline(i GitHubItem, fs []Filter, newerThan time.Time, hidden bool) bool {
	if i.GetState() != "open" {
		return false
	}

	if i.GetUpdatedAt() == i.GetCreatedAt() {
		return false
	}

	if i.GetComments() == 0 {
		return false
	}

	if newerThan.IsZero() {
		return false
	}

	for _, f := range fs {
		if f.TagRegex() != nil {
			if ok, t := matchTag(tag.Tags, f.TagRegex(), f.TagNegate()); ok {
				if t.NeedsTimeline {
					klog.V(1).Infof("#%d - need timeline due to tag %s (negate=%v)", i.GetNumber(), f.TagRegex(), f.TagNegate())
					return true
				}
			}
		}
		if f.Prioritized != "" {
			klog.V(1).Infof("#%d need timeline due to prioritized filter: %s", i.GetNumber(), f.Prioritized)
			return true
		}
	}

	if newerThan.IsZero() || hidden {
		return false
	}

	// Do we need it? Not really. But it's useful for users to see the tags
	return true
}

func needReviews(i GitHubItem, fs []Filter, newerThan time.Time, hidden bool) bool {
	if i.GetState() != "open" {
		return false
	}

	if i.GetUpdatedAt() == i.GetCreatedAt() {
		return false
	}

	if newerThan.IsZero() {
		return false
	}

	for _, f := range fs {
		if f.TagRegex() != nil {
			if ok, t := matchTag(tag.Tags, f.TagRegex(), f.TagNegate()); ok {
				if t.NeedsReviews {
					klog.V(1).Infof("#%d - need reviews due to tag %s (negate=%v)", i.GetNumber(), f.TagRegex(), f.TagNegate())
					return true
				}
			}
		}
	}

	if newerThan.IsZero() {
		return false
	}

	// Do we need it? Not really. But it's useful for users to see the tags
	return true
}

func formatStruct(x interface{}) string {
	s, err := prettyjson.Marshal(x)
	if err == nil {
		return string(s)
	}
	y := strings.Replace(spew.Sdump(x), "\n", "\n|", -1)
	y = strings.Replace(y, ", ", ",\n - ", -1)
	y = strings.Replace(y, "}, ", "},\n", -1)
	return strings.Replace(y, "},\n - ", "},\n", -1)
}
