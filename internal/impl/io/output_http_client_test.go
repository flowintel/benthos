package io

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/benthosdev/benthos/v4/internal/component/metrics"
	"github.com/benthosdev/benthos/v4/internal/log"
	"github.com/benthosdev/benthos/v4/internal/manager/mock"
	"github.com/benthosdev/benthos/v4/internal/message"
	ooutput "github.com/benthosdev/benthos/v4/internal/old/output"
	"github.com/benthosdev/benthos/v4/internal/transaction"
)

func TestHTTPClientMultipartEnabled(t *testing.T) {
	resultChan := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(mediaType, "multipart/"))

		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)

			msgBytes, err := io.ReadAll(p)
			require.NoError(t, err)

			resultChan <- string(msgBytes)
		}
	}))
	defer ts.Close()

	conf := ooutput.NewConfig()
	conf.Type = "http_client"
	conf.HTTPClient.BatchAsMultipart = true
	conf.HTTPClient.URL = ts.URL + "/testpost"

	h, err := newHTTPClientOutput(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	require.NoError(t, err)

	tChan := make(chan message.Transaction)
	require.NoError(t, h.Consume(tChan))

	resChan := make(chan error)
	select {
	case tChan <- message.NewTransaction(message.QuickBatch([][]byte{
		[]byte("PART-A"),
		[]byte("PART-B"),
		[]byte("PART-C"),
	}), resChan):
	case <-time.After(time.Second):
		t.Fatal("Action timed out")
	}

	for _, exp := range []string{
		"PART-A",
		"PART-B",
		"PART-C",
	} {
		select {
		case resMsg := <-resultChan:
			assert.Equal(t, exp, resMsg)
		case <-time.After(time.Second):
			t.Fatal("Action timed out")
		}
	}

	select {
	case res := <-resChan:
		assert.NoError(t, res)
	case <-time.After(time.Second):
		t.Fatal("Action timed out")
	}

	h.CloseAsync()
	require.NoError(t, h.WaitForClose(time.Second))
}

func TestHTTPClientMultipartDisabled(t *testing.T) {
	resultChan := make(chan string, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resBytes, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		resultChan <- string(resBytes)
	}))
	defer ts.Close()

	conf := ooutput.NewConfig()
	conf.Type = "http_client"
	conf.HTTPClient.URL = ts.URL + "/testpost"
	conf.HTTPClient.BatchAsMultipart = false
	conf.HTTPClient.MaxInFlight = 1

	h, err := newHTTPClientOutput(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	require.NoError(t, err)

	tChan := make(chan message.Transaction)
	require.NoError(t, h.Consume(tChan))

	resChan := make(chan error)
	select {
	case tChan <- message.NewTransaction(message.QuickBatch([][]byte{
		[]byte("PART-A"),
		[]byte("PART-B"),
		[]byte("PART-C"),
	}), resChan):
	case <-time.After(time.Second):
		t.Fatal("Action timed out")
	}

	for _, exp := range []string{
		"PART-A",
		"PART-B",
		"PART-C",
	} {
		select {
		case resMsg := <-resultChan:
			assert.Equal(t, exp, resMsg)
		case <-time.After(time.Second):
			t.Fatal("Action timed out")
		}
	}

	select {
	case res := <-resChan:
		assert.NoError(t, res)
	case <-time.After(time.Second):
		t.Fatal("Action timed out")
	}

	h.CloseAsync()
	require.NoError(t, h.WaitForClose(time.Second))
}

func TestHTTPClientRetries(t *testing.T) {
	var reqCount uint32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddUint32(&reqCount, 1)
		http.Error(w, "test error", http.StatusForbidden)
	}))
	defer ts.Close()

	conf := ooutput.NewHTTPClientConfig()
	conf.URL = ts.URL + "/testpost"
	conf.Retry = "1ms"
	conf.NumRetries = 3

	h, err := newHTTPClientWriter(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	if err != nil {
		t.Fatal(err)
	}

	if err = h.WriteWithContext(context.Background(), message.QuickBatch([][]byte{[]byte("test")})); err == nil {
		t.Error("Expected error from end of retries")
	}

	if exp, act := uint32(4), atomic.LoadUint32(&reqCount); exp != act {
		t.Errorf("Wrong count of HTTP attempts: %v != %v", exp, act)
	}

	h.CloseAsync()
	if err = h.WaitForClose(time.Second); err != nil {
		t.Error(err)
	}
}

func TestHTTPClientBasic(t *testing.T) {
	nTestLoops := 1000

	resultChan := make(chan *message.Batch, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := message.QuickBatch(nil)
		defer func() {
			resultChan <- msg
		}()

		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		msg.Append(message.NewPart(b))
	}))
	defer ts.Close()

	conf := ooutput.NewHTTPClientConfig()
	conf.URL = ts.URL + "/testpost"

	h, err := newHTTPClientWriter(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < nTestLoops; i++ {
		testStr := fmt.Sprintf("test%v", i)
		testMsg := message.QuickBatch([][]byte{[]byte(testStr)})

		if err = h.WriteWithContext(context.Background(), testMsg); err != nil {
			t.Error(err)
		}

		select {
		case resMsg := <-resultChan:
			if resMsg.Len() != 1 {
				t.Errorf("Wrong # parts: %v != %v", resMsg.Len(), 1)
				return
			}
			if exp, actual := testStr, string(resMsg.Get(0).Get()); exp != actual {
				t.Errorf("Wrong result, %v != %v", exp, actual)
				return
			}
		case <-time.After(time.Second):
			t.Errorf("Action timed out")
			return
		}
	}

	h.CloseAsync()
	if err = h.WaitForClose(time.Second); err != nil {
		t.Error(err)
	}
}

func TestHTTPClientSyncResponse(t *testing.T) {
	nTestLoops := 1000

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Add("fooheader", "foovalue")
		_, _ = w.Write([]byte("echo: "))
		_, _ = w.Write(b)
	}))
	defer ts.Close()

	conf := ooutput.NewHTTPClientConfig()
	conf.URL = ts.URL + "/testpost"
	conf.PropagateResponse = true

	h, err := newHTTPClientWriter(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < nTestLoops; i++ {
		testStr := fmt.Sprintf("test%v", i)

		resultStore := transaction.NewResultStore()
		testMsg := message.QuickBatch([][]byte{[]byte(testStr)})
		transaction.AddResultStore(testMsg, resultStore)

		require.NoError(t, h.WriteWithContext(context.Background(), testMsg))
		resMsgs := resultStore.Get()
		require.Len(t, resMsgs, 1)

		resMsg := resMsgs[0]
		require.Equal(t, 1, resMsg.Len())
		assert.Equal(t, "echo: "+testStr, string(resMsg.Get(0).Get()))
		assert.Equal(t, "", resMsg.Get(0).MetaGet("fooheader"))
	}

	h.CloseAsync()
	if err = h.WaitForClose(time.Second); err != nil {
		t.Error(err)
	}
}

func TestHTTPClientSyncResponseCopyHeaders(t *testing.T) {
	nTestLoops := 1000

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Add("fooheader", "foovalue")
		_, _ = w.Write([]byte("echo: "))
		_, _ = w.Write(b)
	}))
	defer ts.Close()

	conf := ooutput.NewHTTPClientConfig()
	conf.URL = ts.URL + "/testpost"
	conf.PropagateResponse = true
	conf.ExtractMetadata.IncludePatterns = []string{".*"}

	h, err := newHTTPClientWriter(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < nTestLoops; i++ {
		testStr := fmt.Sprintf("test%v", i)

		resultStore := transaction.NewResultStore()
		testMsg := message.QuickBatch([][]byte{[]byte(testStr)})
		transaction.AddResultStore(testMsg, resultStore)

		require.NoError(t, h.WriteWithContext(context.Background(), testMsg))
		resMsgs := resultStore.Get()
		require.Len(t, resMsgs, 1)

		resMsg := resMsgs[0]
		require.Equal(t, 1, resMsg.Len())
		assert.Equal(t, "echo: "+testStr, string(resMsg.Get(0).Get()))
		assert.Equal(t, "foovalue", resMsg.Get(0).MetaGet("fooheader"))
	}

	h.CloseAsync()
	if err = h.WaitForClose(time.Second); err != nil {
		t.Error(err)
	}
}

func TestHTTPClientMultipart(t *testing.T) {
	nTestLoops := 1000

	resultChan := make(chan *message.Batch, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := message.QuickBatch(nil)
		defer func() {
			resultChan <- msg
		}()

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("Bad media type: %v -> %v", r.Header.Get("Content-Type"), err)
			return
		}

		if strings.HasPrefix(mediaType, "multipart/") {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					return
				}
				msgBytes, err := io.ReadAll(p)
				if err != nil {
					t.Error(err)
					return
				}
				msg.Append(message.NewPart(msgBytes))
			}
		} else {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			msg.Append(message.NewPart(b))
		}
	}))
	defer ts.Close()

	conf := ooutput.NewHTTPClientConfig()
	conf.URL = ts.URL + "/testpost"

	h, err := newHTTPClientWriter(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < nTestLoops; i++ {
		testStr := fmt.Sprintf("test%v", i)
		testMsg := message.QuickBatch([][]byte{
			[]byte(testStr + "PART-A"),
			[]byte(testStr + "PART-B"),
		})

		if err = h.WriteWithContext(context.Background(), testMsg); err != nil {
			t.Error(err)
		}

		select {
		case resMsg := <-resultChan:
			if resMsg.Len() != 2 {
				t.Errorf("Wrong # parts: %v != %v", resMsg.Len(), 2)
				return
			}
			if exp, actual := testStr+"PART-A", string(resMsg.Get(0).Get()); exp != actual {
				t.Errorf("Wrong result, %v != %v", exp, actual)
				return
			}
			if exp, actual := testStr+"PART-B", string(resMsg.Get(1).Get()); exp != actual {
				t.Errorf("Wrong result, %v != %v", exp, actual)
				return
			}
		case <-time.After(time.Second):
			t.Errorf("Action timed out")
			return
		}
	}

	h.CloseAsync()
	if err = h.WaitForClose(time.Second); err != nil {
		t.Error(err)
	}
}
func TestHTTPOutputClientMultipartBody(t *testing.T) {
	nTestLoops := 1000
	resultChan := make(chan *message.Batch, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := message.QuickBatch(nil)
		defer func() {
			resultChan <- msg
		}()

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("Bad media type: %v -> %v", r.Header.Get("Content-Type"), err)
			return
		}

		if strings.HasPrefix(mediaType, "multipart/") {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, err := mr.NextPart()

				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					return
				}
				msgBytes, err := io.ReadAll(p)
				if err != nil {
					t.Error(err)
					return
				}
				msg.Append(message.NewPart(msgBytes))
			}
		}
	}))
	defer ts.Close()

	conf := ooutput.NewHTTPClientConfig()
	conf.URL = ts.URL + "/testpost"
	conf.Multipart = []ooutput.HTTPClientMultipartExpression{
		{
			ContentDisposition: `form-data; name="text"`,
			ContentType:        "text/plain",
			Body:               "PART-A"},
		{
			ContentDisposition: `form-data; name="file1"; filename="a.txt"`,
			ContentType:        "text/plain",
			Body:               "PART-B"},
	}
	h, err := newHTTPClientWriter(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < nTestLoops; i++ {
		if err = h.WriteWithContext(context.Background(), message.QuickBatch([][]byte{[]byte("test")})); err != nil {
			t.Error(err)
		}
		select {
		case resMsg := <-resultChan:
			if resMsg.Len() != len(conf.Multipart) {
				t.Errorf("Wrong # parts: %v != %v", resMsg.Len(), 2)
				return
			}
			if exp, actual := "PART-A", string(resMsg.Get(0).Get()); exp != actual {
				t.Errorf("Wrong result, %v != %v", exp, actual)
				return
			}
			if exp, actual := "PART-B", string(resMsg.Get(1).Get()); exp != actual {
				t.Errorf("Wrong result, %v != %v", exp, actual)
				return
			}
		case <-time.After(time.Second):
			t.Errorf("Action timed out")
			return
		}
	}

	h.CloseAsync()
	if err = h.WaitForClose(time.Second); err != nil {
		t.Error(err)
	}
}

func TestHTTPOutputClientMultipartHeaders(t *testing.T) {
	resultChan := make(chan *message.Batch, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := message.QuickBatch(nil)
		defer func() {
			resultChan <- msg
		}()

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("Bad media type: %v -> %v", r.Header.Get("Content-Type"), err)
			return
		}

		if strings.HasPrefix(mediaType, "multipart/") {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, err := mr.NextPart()

				if err == io.EOF {
					break
				}
				if err != nil {
					t.Error(err)
					return
				}
				a, err := json.Marshal(p.Header)
				if err != nil {
					t.Error(err)
					return
				}
				msg.Append(message.NewPart(a))
			}
		}
	}))
	defer ts.Close()

	conf := ooutput.NewHTTPClientConfig()
	conf.URL = ts.URL + "/testpost"
	conf.Multipart = []ooutput.HTTPClientMultipartExpression{
		{
			ContentDisposition: `form-data; name="text"`,
			ContentType:        "text/plain",
			Body:               "PART-A"},
		{
			ContentDisposition: `form-data; name="file1"; filename="a.txt"`,
			ContentType:        "text/plain",
			Body:               "PART-B"},
	}
	h, err := newHTTPClientWriter(conf, mock.NewManager(), log.Noop(), metrics.Noop())
	if err != nil {
		t.Fatal(err)
	}
	if err = h.WriteWithContext(context.Background(), message.QuickBatch([][]byte{[]byte("test")})); err != nil {
		t.Error(err)
	}
	select {
	case resMsg := <-resultChan:
		for i := range conf.Multipart {
			if resMsg.Len() != len(conf.Multipart) {
				t.Errorf("Wrong # parts: %v != %v", resMsg.Len(), 2)
				return
			}
			mp := make(map[string][]string)
			err := json.Unmarshal(resMsg.Get(i).Get(), &mp)
			if err != nil {
				t.Error(err)
			}
			if exp, actual := conf.Multipart[i].ContentDisposition, mp["Content-Disposition"]; exp != actual[0] {
				t.Errorf("Wrong result, %v != %v", exp, actual)
				return
			}
			if exp, actual := conf.Multipart[i].ContentType, mp["Content-Type"]; exp != actual[0] {
				t.Errorf("Wrong result, %v != %v", exp, actual)
				return
			}
		}
	case <-time.After(time.Second):
		t.Errorf("Action timed out")
		return

	}
	h.CloseAsync()
	if err = h.WaitForClose(time.Second); err != nil {
		t.Error(err)
	}
}