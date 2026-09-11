package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/movsar/tt/internal/api"
)

func TestAPIDetailQuotesOnlyTheRetainedRawBody(t *testing.T) {
	credential := oauthCredentialFixture('J')
	cases := []struct {
		name string
		err  error
		body string
		sent string
	}{
		{
			name: "status body with newline and literal escape",
			body: "a\nb\\nc\"d",
			err:  &api.StatusError{StatusCode: 500, Body: "a\nb\\nc\"d"},
		},
		{
			name: "decode body with controls and invalid UTF-8",
			body: string([]byte{'a', 0x01, 0xff, 'z'}),
			err: &api.DecodeError{
				StatusCode: 200,
				Body:       string([]byte{'a', 0x01, 0xff, 'z'}),
				Err:        errors.New("decoder fragment must stay out of the body quote"),
			},
		},
		{
			name: "incomplete body with fixed classification",
			body: "{}",
			err: &api.DecodeError{
				StatusCode: 200,
				Body:       "{}",
				Err:        api.ErrIncompleteAnswer,
			},
		},
		{
			name: "body after explicit credential removal",
			body: "a" + credential + "b",
			sent: credential,
			err:  &api.StatusError{StatusCode: 401, Body: "a" + credential + "b"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantBody := c.body
			if len(c.sent) >= 16 {
				wantBody = strings.ReplaceAll(wantBody, c.sent, "<the token tt sent>")
			}
			wantQuote := strconv.Quote(wantBody)
			detail := apiDetail(c.err, c.sent)
			if !strings.HasSuffix(detail, ": "+wantQuote) {
				t.Fatal("doctor did not place exactly one raw-body quote after its fixed classification")
			}
			quoted := detail[len(detail)-len(wantQuote):]
			gotBody, err := strconv.Unquote(quoted)
			if err != nil || gotBody != wantBody {
				t.Fatal("doctor body quote did not recover the retained post-redaction bytes")
			}
			assertCredentialMaterialAbsent(t, detail, c.sent)
		})
	}
}

func TestAPIDetailDoesNotRenderArbitraryDecoderProse(t *testing.T) {
	fragment := strings.Repeat("decoder-fragment-", 2)
	detail := apiDetail(&api.DecodeError{
		StatusCode: 200,
		Body:       "body",
		Err:        errors.New(fragment),
	}, "")
	if strings.Contains(detail, fragment) {
		t.Fatal("doctor copied arbitrary decoder prose outside the body quote")
	}
}
