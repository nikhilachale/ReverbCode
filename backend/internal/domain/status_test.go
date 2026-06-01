package domain

import "testing"

func rec(activity ActivityState, terminated bool) SessionRecord {
	return SessionRecord{Activity: ActivitySubstate{State: activity}, IsTerminated: terminated}
}

func TestDeriveStatusFromSessionFactsAndPR(t *testing.T) {
	tests := []struct {
		name string
		rec  SessionRecord
		pr   PRFacts
		want SessionStatus
	}{
		{"terminated", rec(ActivityExited, true), PRFacts{}, StatusTerminated},
		{"merged-pr", rec(ActivityIdle, true), PRFacts{Exists: true, Merged: true}, StatusMerged},
		{"needs-input", rec(ActivityWaitingInput, false), PRFacts{Exists: true, CI: CIFailing}, StatusNeedsInput},
		{"blocked", rec(ActivityBlocked, false), PRFacts{Exists: true, CI: CIFailing}, StatusStuck},
		{"ci-failed", rec(ActivityIdle, false), PRFacts{Exists: true, CI: CIFailing}, StatusCIFailed},
		{"draft", rec(ActivityIdle, false), PRFacts{Exists: true, Draft: true}, StatusDraft},
		{"changes-requested", rec(ActivityIdle, false), PRFacts{Exists: true, Review: ReviewChangesRequest}, StatusChangesRequested},
		{"mergeable", rec(ActivityIdle, false), PRFacts{Exists: true, Mergeability: MergeMergeable}, StatusMergeable},
		{"approved", rec(ActivityIdle, false), PRFacts{Exists: true, Review: ReviewApproved}, StatusApproved},
		{"review-pending", rec(ActivityIdle, false), PRFacts{Exists: true, Review: ReviewRequired}, StatusReviewPending},
		{"pr-open", rec(ActivityIdle, false), PRFacts{Exists: true}, StatusPROpen},
		{"working", rec(ActivityActive, false), PRFacts{}, StatusWorking},
		{"idle", rec(ActivityIdle, false), PRFacts{}, StatusIdle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveStatus(tt.rec, tt.pr); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDisplayPRPrefersActivePR(t *testing.T) {
	prs := []PRFacts{
		{Exists: true, URL: "closed", Closed: true, CI: CIPassing},
		{Exists: true, URL: "open", CI: CIFailing},
	}
	if got := DisplayPR(prs); got.URL != "open" {
		t.Fatalf("got %+v", got)
	}
}

func TestDisplayPRFallsBackToHistoricalPR(t *testing.T) {
	prs := []PRFacts{
		{Exists: true, URL: "closed", Closed: true},
		{Exists: true, URL: "merged", Merged: true},
	}
	if got := DisplayPR(prs); got.URL != "closed" {
		t.Fatalf("got %+v", got)
	}
}
