// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package kibana

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/elastic-agent-libs/kibana"
	"github.com/elastic/elastic-agent-libs/version"
)

func TestNewConnectingClientFrom(t *testing.T) {
	c := NewConnectingClient(mockCfg)
	require.NotNil(t, c)
	assert.Nil(t, c.(*ConnectingClient).client)
	assert.Equal(t, mockCfg, c.(*ConnectingClient).cfg)
}

func TestNewConnectingClientWithAPIKey(t *testing.T) {
	var headers http.Header
	var h http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) { headers = r.Header }
	srv := httptest.NewServer(h)
	defer srv.Close()

	cfg := kibana.ClientConfig{
		APIKey:        "foo-id:bar-apikey",
		Host:          srv.URL,
		IgnoreVersion: true,
	}
	conn := &ConnectingClient{cfg: cfg}
	require.NotNil(t, conn)
	err := conn.connect()
	require.NoError(t, err)

	resp, err := conn.Send(context.Background(), http.MethodGet, "", nil, nil, nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, "ApiKey Zm9vLWlkOmJhci1hcGlrZXk=", headers.Get("Authorization"))
}

func TestConnectingClient_Send(t *testing.T) {
	t.Run("Send", func(t *testing.T) {
		c := mockClient()
		r, err := c.Send(context.Background(), http.MethodGet, "", nil, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, mockBody, r.Body)
		assert.Equal(t, mockStatus, r.StatusCode)
	})

	t.Run("SendError", func(t *testing.T) {
		c := NewConnectingClient(mockCfg)
		r, err := c.Send(context.Background(), http.MethodGet, "", nil, nil, nil)
		require.Error(t, err)
		assert.Equal(t, err, errNotConnected)
		assert.Nil(t, r)
	})
}

func TestConnectingClient_GetVersion(t *testing.T) {
	t.Run("GetVersion", func(t *testing.T) {
		c := mockClient()
		v, err := c.GetVersion(context.Background())
		require.NoError(t, err)
		assert.Equal(t, mockVersion, v)
	})

	t.Run("GetVersionError", func(t *testing.T) {
		c := NewConnectingClient(mockCfg)
		v, err := c.GetVersion(context.Background())
		require.Error(t, err)
		assert.Equal(t, err, errNotConnected)
		assert.Equal(t, version.V{}, v)
	})
}

func TestConnectingClient_SupportsVersion(t *testing.T) {
	t.Run("SupportsVersionTrue", func(t *testing.T) {
		c := mockClient()
		s, err := c.SupportsVersion(context.Background(), version.MustNew("7.3.0"), false)
		require.NoError(t, err)
		assert.True(t, s)
	})
	t.Run("SupportsVersionFalse", func(t *testing.T) {
		c := mockClient()
		s, err := c.SupportsVersion(context.Background(), version.MustNew("7.4.0"), false)
		require.NoError(t, err)
		assert.False(t, s)
	})

	t.Run("SupportsVersionError", func(t *testing.T) {
		c := NewConnectingClient(mockCfg)
		s, err := c.SupportsVersion(context.Background(), version.MustNew("7.3.0"), false)
		require.Error(t, err)
		assert.Equal(t, err, errNotConnected)
		assert.False(t, s)
	})
}

type rt struct {
	resp *http.Response
}

var (
	mockCfg = kibana.ClientConfig{
		Host: "non-existing",
	}
	mockBody    = io.NopCloser(strings.NewReader(`{"response": "ok"}`))
	mockStatus  = http.StatusOK
	mockVersion = *version.MustNew("7.3.0")
)

// RoundTrip implements the Round Tripper interface
func (rt rt) RoundTrip(r *http.Request) (*http.Response, error) {
	return rt.resp, nil
}
func mockClient() *ConnectingClient {
	return &ConnectingClient{client: &kibana.Client{
		Connection: kibana.Connection{
			HTTP: &http.Client{
				Transport: rt{resp: &http.Response{
					StatusCode: mockStatus,
					Body:       mockBody}},
			},
			Version: mockVersion,
		},
	}}
}
