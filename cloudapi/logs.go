/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2020 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package cloudapi

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mailru/easyjson"
	"github.com/sirupsen/logrus"
)

//go:generate easyjson -pkg -no_std_marshalers -gen_build_flags -mod=mod .

//easyjson:json
type msg struct {
	Streams        []msgStreams        `json:"streams"`
	DroppedEntries []msgDroppedEntries `json:"dropped_entries"`
}

//easyjson:json
type msgStreams struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"` // this can be optimized
}

func (ms msgStreams) LatestTimestamp() (ts int64) {
	if len(ms.Values) < 1 {
		return
	}
	for i := 0; i < len(ms.Values); i++ {
		raw := ms.Values[i][0]
		unix, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return
		}
		if unix > ts {
			ts = unix
		}
	}
	return
}

//easyjson:json
type msgDroppedEntries struct {
	Labels    map[string]string `json:"labels"`
	Timestamp string            `json:"timestamp"`
}

func (m *msg) Log(logger logrus.FieldLogger) {
	var level string

	for _, stream := range m.Streams {
		fields := labelsToLogrusFields(stream.Stream)
		var ok bool
		if level, ok = stream.Stream["level"]; ok {
			delete(fields, "level")
		}

		for _, value := range stream.Values {
			nsec, _ := strconv.Atoi(value[0])
			e := logger.WithFields(fields).WithTime(time.Unix(0, int64(nsec)))
			lvl, err := logrus.ParseLevel(level)
			if err != nil {
				e.Info(value[1])
				e.Warn("last message had unknown level " + level)
			} else {
				e.Log(lvl, value[1])
			}
		}
	}

	for _, dropped := range m.DroppedEntries {
		nsec, _ := strconv.Atoi(dropped.Timestamp)
		logger.WithFields(labelsToLogrusFields(dropped.Labels)).WithTime(time.Unix(0, int64(nsec))).Warn("dropped")
	}
}

func labelsToLogrusFields(labels map[string]string) logrus.Fields {
	fields := make(logrus.Fields, len(labels))

	for key, val := range labels {
		fields[key] = val
	}

	return fields
}

func (c *Config) logtailConn(ctx context.Context, referenceID string, since time.Time) (*websocket.Conn, error) {
	u, err := url.Parse(c.LogsTailURL.String)
	if err != nil {
		return nil, fmt.Errorf("couldn't parse cloud logs host %w", err)
	}

	u.RawQuery = fmt.Sprintf(`query={test_run_id="%s"}&start=%d`, referenceID, since.UnixNano())

	headers := make(http.Header)
	headers.Add("Sec-WebSocket-Protocol", "token="+c.Token.String)

	var conn *websocket.Conn
	err = retry(sleeperFunc(time.Sleep), 3, 5*time.Second, 2*time.Minute, func() (err error) {
		// We don't need to close the http body or use it for anything until we want to actually log
		// what the server returned as body when it errors out
		conn, _, err = websocket.DefaultDialer.DialContext(ctx, u.String(), headers) //nolint:bodyclose
		return err
	})
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// StreamLogsToLogger streams the logs for the configured test to the provided logger until ctx is
// Done or an error occurs.
func (c *Config) StreamLogsToLogger(
	ctx context.Context, logger logrus.FieldLogger, referenceID string, tailFrom time.Duration,
) error {
	var mconn sync.Mutex

	conn, err := c.logtailConn(ctx, referenceID, time.Now().Add(-tailFrom))
	if err != nil {
		return err
	}

	go func() {
		<-ctx.Done()

		mconn.Lock()
		defer mconn.Unlock()

		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "closing"),
			time.Now().Add(time.Second))

		_ = conn.Close()
	}()

	msgBuffer := make(chan []byte, 10)
	defer close(msgBuffer)

	latest := &timestampTrack{}
	go func() {
		for message := range msgBuffer {
			var m msg
			err := easyjson.Unmarshal(message, &m)
			if err != nil {
				logger.WithError(err).Errorf("couldn't unmarshal a message from the cloud: %s", string(message))

				continue
			}
			m.Log(logger)

			var ts int64
			for _, stream := range m.Streams {
				sts := stream.LatestTimestamp()
				if sts > ts {
					ts = sts
				}
			}
			latest.Set(ts)
		}
	}()

	for {
		_, message, err := conn.ReadMessage()
		select { // check if we should stop before continuing
		case <-ctx.Done():
			return nil
		default:
		}

		if err != nil {
			logger.WithError(err).Warn("error reading a log message from the cloud, trying to establish a fresh connection with the logs service...")

			newconn, errd := c.logtailConn(ctx, referenceID, latest.TimeOrNow())
			if errd == nil {
				mconn.Lock()
				conn = newconn
				mconn.Unlock()
				continue
			}

			// return the main error
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case msgBuffer <- message:
		}
	}
}

// timstampTrack is a safe-concurrent tracker
// of the latest/most recent seen timestamp value.
type timestampTrack struct {
	// ts is timestmap in unix nano format
	ts int64
}

// TimeOrNow returns as Time the latest tracked value
// or Now as the default value.
func (tt *timestampTrack) TimeOrNow() (t time.Time) {
	t = time.Now()
	ts := atomic.LoadInt64(&tt.ts)
	if ts > 0 {
		t = time.Unix(0, int64(ts))
	}
	return
}

// Set sets the tracked timestamp value.
func (tst *timestampTrack) Set(ts int64) {
	if ts < 1 {
		return
	}
	atomic.StoreInt64(&tst.ts, ts)
}

// sleeper represents an abstraction for waiting an amount of time.
type sleeper interface {
	Sleep(d time.Duration)
}

// sleeperFunc uses the underhood function for implementing the wait operation.
type sleeperFunc func(time.Duration)

func (sfn sleeperFunc) Sleep(d time.Duration) {
	sfn(d)
}

// retry retries to execute a provided function until it isn't successful
// or the maximum number of attempts is hit. It waits the specified interval
// between the latest iteration and the next retry.
// Interval is used as the base to compute an exponential backoff,
// if the computed interval overtakes the max interval then max will be used.
func retry(s sleeper, attempts uint, interval, max time.Duration, do func() error) (err error) {
	baseInterval := math.Abs(interval.Truncate(time.Second).Seconds())
	r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec

	for i := 0; i < int(attempts); i++ {
		if i > 0 {
			// wait = (interval ^ i) + random milliseconds
			wait := time.Duration(math.Pow(baseInterval, float64(i))) * time.Second
			wait += time.Duration(r.Int63n(1000)) * time.Millisecond

			if wait > max {
				wait = max
			}
			s.Sleep(wait)
		}
		err = do()
		if err == nil {
			return nil
		}
	}
	return
}
