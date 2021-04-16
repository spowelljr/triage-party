package hubbub

import (
	"context"
	"time"

	"github.com/google/triage-party/pkg/constants"
	"github.com/google/triage-party/pkg/provider"
	"github.com/google/triage-party/pkg/tag"
	"k8s.io/klog/v2"
)

func (h *Engine) analyzeIssue(ctx context.Context, i *provider.Issue, sp provider.SearchParams, age time.Time, latestIssueUpdate time.Time) *Conversation {
	// Workaround API inconsistency: issues use a list of labels, prs a list of label pointers
	labels := []*provider.Label{}
	for _, l := range i.Labels {
		l := l
		labels = append(labels, l)
	}

	if !preFetchMatch(i, labels, sp.Filters) {
		klog.V(1).Infof("#%d - %q did not match item filter: %s", i.GetNumber(), i.GetTitle(), sp.Filters)
		return nil
	}

	klog.V(1).Infof("#%d - %q made it past pre-fetch: %s", i.GetNumber(), i.GetTitle(), sp.Filters)

	fetchComments := false
	if needComments(i, sp.Filters) && i.GetComments() > 0 {
		klog.V(1).Infof("#%d - %q: need comments for final filtering", i.GetNumber(), i.GetTitle())
		fetchComments = !sp.NewerThan.IsZero()
	}

	sp.IssueNumber = i.GetNumber()
	sp.NewerThan = h.mtime(i)
	sp.Fetch = fetchComments

	comments, _, err := h.cachedIssueComments(ctx, sp)
	if err != nil {
		klog.Errorf("comments: %v", err)
	}

	co := h.IssueSummary(i, comments, age)
	co.Labels = labels

	co.Similar = h.FindSimilar(co)
	if len(co.Similar) > 0 {
		co.Tags[tag.Similar] = true
	}

	if !postFetchMatch(co, sp.Filters) {
		klog.V(1).Infof("#%d - %q did not match post-fetch filter: %s", i.GetNumber(), i.GetTitle(), sp.Filters)
		return nil
	}
	klog.V(1).Infof("#%d - %q made it past post-fetch: %s", i.GetNumber(), i.GetTitle(), sp.Filters)

	updatedAt := h.mtime(i)
	var timeline []*provider.Timeline
	fetchTimeline := false
	if needTimeline(i, sp.Filters, false, sp.Hidden) {
		fetchTimeline = !sp.NewerThan.IsZero()
	}

	sp.IssueNumber = i.GetNumber()
	sp.Fetch = fetchTimeline
	sp.UpdateAt = updatedAt

	timeline, err = h.cachedTimeline(ctx, sp)
	if err != nil {
		klog.Errorf("timeline: %v", err)
	}

	h.addEvents(ctx, sp, co, timeline)

	// Some labels are judged by linked PR state. Ensure that they are updated to the same timestamp.
	fetchReviews := false
	if needReviews(i, sp.Filters, sp.Hidden) && len(co.PullRequestRefs) > 0 {
		fetchReviews = !sp.NewerThan.IsZero()
	}
	sp.NewerThan = latestIssueUpdate
	sp.Fetch = fetchReviews
	co.PullRequestRefs = h.updateLinkedPRs(ctx, sp, co)

	if !postEventsMatch(co, sp.Filters) {
		klog.V(1).Infof("#%d - %q did not match post-events filter: %s", i.GetNumber(), i.GetTitle(), sp.Filters)
		return nil
	}

	klog.V(1).Infof("#%d - %q made it past post-events: %s", i.GetNumber(), i.GetTitle(), sp.Filters)
	return co
}

func (h *Engine) analyzePR(ctx context.Context, pr *provider.PullRequest, sp provider.SearchParams, age time.Time) *Conversation {
	if !preFetchMatch(pr, pr.Labels, sp.Filters) {
		return nil
	}

	var timeline []*provider.Timeline
	var reviews []*provider.PullRequestReview
	var comments []*provider.Comment

	fetchComments := false
	if needComments(pr, sp.Filters) {
		fetchComments = !sp.NewerThan.IsZero()
	}

	sp.IssueNumber = pr.GetNumber()
	sp.NewerThan = h.mtime(pr)
	sp.Fetch = fetchComments

	comments, _, err := h.prComments(ctx, sp)
	if err != nil {
		klog.Errorf("comments: %v", err)
	}

	fetchTimeline := false
	if needTimeline(pr, sp.Filters, true, sp.Hidden) {
		fetchTimeline = !sp.NewerThan.IsZero()
	}

	sp.IssueNumber = pr.GetNumber()
	sp.NewerThan = h.mtime(pr)
	sp.Fetch = fetchTimeline

	timeline, err = h.cachedTimeline(ctx, sp)
	if err != nil {
		klog.Errorf("timeline: %v", err)
	}

	fetchReviews := false
	if needReviews(pr, sp.Filters, sp.Hidden) {
		fetchReviews = !sp.NewerThan.IsZero()
	}

	sp.IssueNumber = pr.GetNumber()
	sp.NewerThan = h.mtime(pr)
	sp.Fetch = fetchReviews

	reviews, _, err = h.cachedReviews(ctx, sp)
	if err != nil {
		klog.Errorf("reviews: %v", err)
	}

	if h.debug[pr.GetNumber()] {
		klog.Errorf("*** Debug PR timeline #%d:\n%s", pr.GetNumber(), formatStruct(timeline))
	}

	sp.Fetch = !sp.NewerThan.IsZero()
	sp.Age = age

	co := h.PRSummary(ctx, sp, pr, comments, timeline, reviews)
	co.Labels = pr.Labels
	co.Similar = h.FindSimilar(co)
	if len(co.Similar) > 0 {
		co.Tags[tag.Similar] = true
	}

	if !postFetchMatch(co, sp.Filters) {
		klog.V(4).Infof("PR #%d did not pass postFetchMatch with filter: %v", pr.GetNumber(), sp.Filters)
		return nil
	}

	if !postEventsMatch(co, sp.Filters) {
		klog.V(1).Infof("#%d - %q did not match post-events filter: %s", pr.GetNumber(), pr.GetTitle(), sp.Filters)
		return nil
	}

	return co
}

func needReviews(i provider.IItem, fs []provider.Filter, hidden bool) bool {
	if (i.GetState() != constants.OpenState) && (i.GetState() != constants.OpenedState) {
		return false
	}

	if i.GetUpdatedAt() == i.GetCreatedAt() {
		return false
	}

	if hidden {
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

	return true
}

func needComments(i provider.IItem, fs []provider.Filter) bool {
	for _, f := range fs {
		if f.TagRegex() != nil {
			if ok, t := matchTag(tag.Tags, f.TagRegex(), f.TagNegate()); ok {
				if t.NeedsComments {
					klog.Infof("#%d - need comments due to tag %s (negate=%v)", i.GetNumber(), f.TagRegex(), f.TagNegate())
					return true
				}
			}
		}

		if f.ClosedCommenters != "" || f.ClosedComments != "" {
			klog.Infof("#%d - need comments due to closed comments", i.GetNumber())
			return true
		}

		if f.Responded != "" || f.Commenters != "" {
			klog.Infof("#%d - need comments due to responded/commenters filter", i.GetNumber())
			return true
		}
	}

	return (i.GetState() == constants.OpenState) || (i.GetState() == constants.OpenedState)
}

func needTimeline(i provider.IItem, fs []provider.Filter, pr bool, hidden bool) bool {
	if i.GetMilestone() != nil {
		return true
	}

	if (i.GetState() != constants.OpenState) && (i.GetState() != constants.OpenedState) {
		return false
	}

	if i.GetUpdatedAt() == i.GetCreatedAt() {
		return false
	}

	if pr {
		return true
	}

	for _, f := range fs {
		if f.TagRegex() != nil {
			if ok, t := matchTag(tag.Tags, f.TagRegex(), f.TagNegate()); ok {
				if t.NeedsTimeline {
					return true
				}
			}
		}
		if f.Prioritized != "" {
			return true
		}
	}

	return !hidden
}
