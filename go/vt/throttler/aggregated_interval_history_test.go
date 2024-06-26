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

package throttler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAggregatedIntervalHistory(t *testing.T) {
	h := newAggregatedIntervalHistory(10, 1*time.Second, 2)
	h.addPerThread(0, record{sinceZero(0 * time.Second), 1000})
	h.addPerThread(1, record{sinceZero(0 * time.Second), 2000})

	got := h.average(sinceZero(250*time.Millisecond), sinceZero(750*time.Millisecond))
	assert.Equal(t, 3000.0, got)
}
