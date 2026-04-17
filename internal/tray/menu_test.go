package tray

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStateMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state string
		want  string
	}{
		{"Connected", "●"},
		{"Starting", "◐"},
		{"Reconnecting", "◐"},
		{"Failed", "✗"},
		{"Stopping", "◌"},
		{"Disconnected", "○"},
		{"", "○"},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, stateMarker(tt.state))
		})
	}
}

func TestServiceLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		svc  *serviceEntry
		want string
	}{
		{
			name: "connected",
			svc:  &serviceEntry{dnsName: "rds.dev", localPort: 5432, state: "Connected"},
			want: "●  rds.dev:5432",
		},
		{
			name: "connected missing port",
			svc:  &serviceEntry{dnsName: "rds.dev", state: "Connected"},
			want: "●  rds.dev",
		},
		{
			name: "failed with error",
			svc:  &serviceEntry{dnsName: "rds.dev", localPort: 5432, state: "Failed", err: "credentials expired"},
			want: "✗  rds.dev:5432  — credentials expired",
		},
		{
			name: "failed no error",
			svc:  &serviceEntry{dnsName: "rds.dev", localPort: 5432, state: "Failed"},
			want: "✗  rds.dev:5432  — failed",
		},
		{
			name: "starting",
			svc:  &serviceEntry{dnsName: "opensearch.dev", localPort: 9443, state: "Starting"},
			want: "◐  opensearch.dev:9443",
		},
		{
			name: "disconnected",
			svc:  &serviceEntry{dnsName: "kibana.dev", localPort: 5601, state: "Disconnected"},
			want: "○  kibana.dev:5601",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, serviceLabel(tt.svc))
		})
	}
}

func TestAggregateState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		profiles map[string]*profileEntry
		want     iconState
	}{
		{
			name:     "no profiles → idle",
			profiles: map[string]*profileEntry{},
			want:     stateIdle,
		},
		{
			name: "all connected → ok",
			profiles: map[string]*profileEntry{
				"dev": {
					services: map[string]*serviceEntry{
						"a": {state: "Connected"},
						"b": {state: "Connected"},
					},
				},
			},
			want: stateOK,
		},
		{
			name: "one reconnecting → warn",
			profiles: map[string]*profileEntry{
				"dev": {
					services: map[string]*serviceEntry{
						"a": {state: "Connected"},
						"b": {state: "Reconnecting"},
					},
				},
			},
			want: stateWarn,
		},
		{
			name: "some connected some disconnected → partial",
			profiles: map[string]*profileEntry{
				"dev": {
					services: map[string]*serviceEntry{
						"a": {state: "Connected"},
						"b": {state: "Disconnected"},
					},
				},
			},
			want: statePartial,
		},
		{
			name: "all disconnected → idle",
			profiles: map[string]*profileEntry{
				"dev": {
					services: map[string]*serviceEntry{
						"a": {state: "Disconnected"},
						"b": {state: "Disconnected"},
					},
				},
			},
			want: stateIdle,
		},
		{
			name: "one failed → err",
			profiles: map[string]*profileEntry{
				"dev": {
					services: map[string]*serviceEntry{
						"a": {state: "Connected"},
						"b": {state: "Failed"},
					},
				},
			},
			want: stateErr,
		},
		{
			name: "creds expired dominates",
			profiles: map[string]*profileEntry{
				"dev": {
					credsExpired: true,
					services: map[string]*serviceEntry{
						"a": {state: "Connected"},
					},
				},
			},
			want: stateErr,
		},
		{
			name: "only starting → warn",
			profiles: map[string]*profileEntry{
				"dev": {
					services: map[string]*serviceEntry{
						"a": {state: "Starting"},
					},
				},
			},
			want: stateWarn,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := newTray("/tmp/x")
			tr.profiles = tt.profiles
			assert.Equal(t, tt.want, tr.aggregateStateLocked())
		})
	}
}

func TestIconFor(t *testing.T) {
	t.Parallel()

	// Just assert each returns non-empty distinct bytes — real validation
	// is visual. This guards against accidentally embedding the wrong file
	// or an empty slice.
	assert.NotEmpty(t, iconFor(stateIdle))
	assert.NotEmpty(t, iconFor(stateOK))
	assert.NotEmpty(t, iconFor(statePartial))
	assert.NotEmpty(t, iconFor(stateWarn))
	assert.NotEmpty(t, iconFor(stateErr))
	// And the default fallback path returns idle.
	assert.Equal(t, iconFor(stateIdle), iconFor(iconState(99)))
}
