package daemon

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Kwutzke/holepunch/internal/engine"
	"github.com/Kwutzke/holepunch/internal/state"
)

func TestEventToEnvelope(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		evt  engine.Event
		want *EventEnvelope
	}{
		{
			name: "LogEntry",
			evt:  engine.LogEntry{Level: "info", Message: "hi", Profile: "dev", Service: "rds", Time: ts},
			want: &EventEnvelope{
				Kind: EnvelopeKindLog,
				Log:  &LogEntryDTO{Level: "info", Message: "hi", Profile: "dev", Service: "rds", Time: ts},
			},
		},
		{
			name: "ServiceStateChanged with error",
			evt: engine.ServiceStateChanged{
				Profile: "dev", ServiceName: "rds", DNSName: "rds.dev",
				From: state.Starting, To: state.Failed,
				Error: errors.New("boom"), Timestamp: ts,
			},
			want: &EventEnvelope{
				Kind: EnvelopeKindState,
				State: &ServiceStateChangedDTO{
					Profile: "dev", ServiceName: "rds", DNSName: "rds.dev",
					From: "Starting", To: "Failed", Error: "boom", Timestamp: ts,
				},
			},
		},
		{
			name: "ServiceStateChanged no error",
			evt: engine.ServiceStateChanged{
				Profile: "dev", ServiceName: "rds",
				From: state.Starting, To: state.Connected, Timestamp: ts,
			},
			want: &EventEnvelope{
				Kind: EnvelopeKindState,
				State: &ServiceStateChangedDTO{
					Profile: "dev", ServiceName: "rds",
					From: "Starting", To: "Connected", Timestamp: ts,
				},
			},
		},
		{
			name: "CredentialsExpired",
			evt: engine.CredentialsExpired{
				Profile: "dev", AWSProfile: "dev-sso", ServiceName: "rds",
				Detail: "ExpiredTokenException", Timestamp: ts,
			},
			want: &EventEnvelope{
				Kind: EnvelopeKindCredsExpired,
				Creds: &CredentialsExpiredDTO{
					Profile: "dev", AWSProfile: "dev-sso", ServiceName: "rds",
					Detail: "ExpiredTokenException", Timestamp: ts,
				},
			},
		},
		{
			name: "ProfileDone",
			evt:  engine.ProfileDone{Profile: "dev", Timestamp: ts},
			want: &EventEnvelope{
				Kind:        EnvelopeKindProfileDone,
				ProfileDone: &ProfileDoneDTO{Profile: "dev", Timestamp: ts},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := eventToEnvelope(tt.evt)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEnvelopeJSONRoundTrip(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	original := Response{
		OK: true,
		Envelope: &EventEnvelope{
			Kind: EnvelopeKindCredsExpired,
			Creds: &CredentialsExpiredDTO{
				Profile: "dev", AWSProfile: "dev-sso",
				ServiceName: "rds", Detail: "token expired", Timestamp: ts,
			},
		},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded Response
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, original, decoded)
}
