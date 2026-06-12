package notification

import (
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type intent struct {
	Type      domain.NotificationType
	SessionID domain.SessionID
	ProjectID domain.ProjectID
	PRURL     string
	CreatedAt time.Time

	SessionDisplayName string
	PRNumber           int
	PRTitle            string
	PRSourceBranch     string
	PRTargetBranch     string
	Provider           string
	Repo               string
}

func enrich(in intent) (domain.NotificationRecord, error) {
	rec := domain.NotificationRecord{
		SessionID: in.SessionID,
		ProjectID: in.ProjectID,
		PRURL:     strings.TrimSpace(in.PRURL),
		Type:      in.Type,
		Status:    domain.NotificationUnread,
		CreatedAt: in.CreatedAt,
	}
	if !in.Type.Valid() {
		return domain.NotificationRecord{}, domain.ErrInvalidNotificationType
	}
	if in.Type != domain.NotificationNeedsInput && rec.PRURL == "" {
		return domain.NotificationRecord{}, domain.ErrInvalidNotificationRecord
	}
	rec.Title = titleForIntent(in)
	rec.Body = bodyForIntent(in)
	if err := rec.Validate(); err != nil {
		return domain.NotificationRecord{}, err
	}
	return rec, nil
}

func titleForIntent(in intent) string {
	switch in.Type {
	case domain.NotificationNeedsInput:
		return fmt.Sprintf("%s needs input", sessionLabel(in))
	case domain.NotificationReadyToMerge:
		return fmt.Sprintf("%s is ready to merge", prLabel(in))
	case domain.NotificationPRMerged:
		return fmt.Sprintf("%s was merged", prLabel(in))
	case domain.NotificationPRClosedUnmerged:
		return fmt.Sprintf("%s was closed without merging", prLabel(in))
	default:
		return "Notification"
	}
}

func bodyForIntent(in intent) string {
	switch in.Type {
	case domain.NotificationNeedsInput:
		return "The agent is waiting for your response."
	case domain.NotificationReadyToMerge:
		if s := sessionLabel(in); s != "session" {
			return fmt.Sprintf("%s has no known blocking CI or review feedback.", s)
		}
		return "The pull request has no known blocking CI or review feedback."
	case domain.NotificationPRMerged:
		if title := strings.TrimSpace(in.PRTitle); title != "" {
			return fmt.Sprintf("%s was merged.", title)
		}
		return "The pull request was merged."
	case domain.NotificationPRClosedUnmerged:
		if title := strings.TrimSpace(in.PRTitle); title != "" {
			return fmt.Sprintf("%s was closed without merging.", title)
		}
		return "The pull request was closed without merging."
	default:
		return ""
	}
}

func sessionLabel(in intent) string {
	if v := strings.TrimSpace(in.SessionDisplayName); v != "" {
		return v
	}
	if in.SessionID != "" {
		return string(in.SessionID)
	}
	return "session"
}

func prLabel(in intent) string {
	if in.PRNumber > 0 {
		return fmt.Sprintf("PR #%d", in.PRNumber)
	}
	if title := strings.TrimSpace(in.PRTitle); title != "" {
		return "PR " + title
	}
	return "PR"
}
