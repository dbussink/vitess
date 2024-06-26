/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tabletserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"vitess.io/vitess/go/vt/sqlparser"
)

func TestLiveQueryzHandlerJSON(t *testing.T) {
	resp := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/livequeryz/?format=json", nil)

	queryList := NewQueryList("test", sqlparser.NewTestParser())
	err := queryList.Add(NewQueryDetail(context.Background(), &testConn{id: 1}))
	require.NoError(t, err)
	err = queryList.Add(NewQueryDetail(context.Background(), &testConn{id: 2}))
	require.NoError(t, err)

	livequeryzHandler([]*QueryList{queryList}, resp, req)
}

func TestLiveQueryzHandlerHTTP(t *testing.T) {
	resp := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/livequeryz/", nil)

	queryList := NewQueryList("test", sqlparser.NewTestParser())
	err := queryList.Add(NewQueryDetail(context.Background(), &testConn{id: 1}))
	require.NoError(t, err)
	err = queryList.Add(NewQueryDetail(context.Background(), &testConn{id: 2}))
	require.NoError(t, err)

	livequeryzHandler([]*QueryList{queryList}, resp, req)
}

func TestLiveQueryzHandlerHTTPFailedInvalidForm(t *testing.T) {
	resp := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/livequeryz/", nil)

	livequeryzHandler([]*QueryList{NewQueryList("test", sqlparser.NewTestParser())}, resp, req)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("http call should fail and return code: %d, but got: %d",
			http.StatusInternalServerError, resp.Code)
	}
}

func TestLiveQueryzHandlerTerminateConn(t *testing.T) {
	resp := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/livequeryz//terminate?connID=1", nil)

	queryList := NewQueryList("test", sqlparser.NewTestParser())
	testConn := &testConn{id: 1}
	err := queryList.Add(NewQueryDetail(context.Background(), testConn))
	require.NoError(t, err)
	if testConn.IsKilled() {
		t.Fatalf("conn should still be alive")
	}
	livequeryzTerminateHandler([]*QueryList{queryList}, resp, req)
	if !testConn.IsKilled() {
		t.Fatalf("conn should be killed")
	}
}

func TestLiveQueryzHandlerTerminateFailedInvalidConnID(t *testing.T) {
	resp := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/livequeryz//terminate?connID=invalid", nil)

	livequeryzTerminateHandler([]*QueryList{NewQueryList("test", sqlparser.NewTestParser())}, resp, req)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("http call should fail and return code: %d, but got: %d",
			http.StatusInternalServerError, resp.Code)
	}
}

func TestLiveQueryzHandlerTerminateFailedInvalidForm(t *testing.T) {
	resp := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/livequeryz//terminate?inva+lid=2", nil)

	livequeryzTerminateHandler([]*QueryList{NewQueryList("test", sqlparser.NewTestParser())}, resp, req)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("http call should fail and return code: %d, but got: %d",
			http.StatusInternalServerError, resp.Code)
	}
}
