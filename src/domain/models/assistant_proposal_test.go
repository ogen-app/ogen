package models

import (
	"testing"
	"time"
)

func TestAssistantProposal_Open(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		status  AssistantProposalStatus
		expires time.Time
		want    bool
	}{
		{"pending, not expired", ProposalPending, now.Add(time.Minute), true},
		{"pending, expires now", ProposalPending, now, false},
		{"pending, expired", ProposalPending, now.Add(-time.Minute), false},
		{"confirmed", ProposalConfirmed, now.Add(time.Hour), false},
		{"dismissed", ProposalDismissed, now.Add(time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &AssistantProposal{Status: tc.status, ExpiresAt: tc.expires}
			if got := p.Open(now); got != tc.want {
				t.Fatalf("Open = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestAssistantProposal_Option(t *testing.T) {
	p := &AssistantProposal{Options: []ProposalOption{{ID: "fill", Title: "Fill the goal"}, {ID: "ideas", Title: "Save as ideas"}}}
	if o, ok := p.Option("ideas"); !ok || o.Title != "Save as ideas" {
		t.Fatalf("Option(ideas) = %+v, %t", o, ok)
	}
	if _, ok := p.Option("raise"); ok {
		t.Fatal("Option found an id that is not offered")
	}
}
