package whisper

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Fake answers from a script, in memory, and records what it was asked. It is
// what an application's tests pass where production passes *Client.
//
//	fake := whisper.NewFake(whisper.Answer{Text: "a ferrugem apareceu no talhão"})
//	svc := services.NewAssistant(db, fake)
//	...
//	fake.Requests()[0].MIMEType // what the service sent
type Fake struct {
	mu       sync.Mutex
	script   []Answer
	requests []Audio
}

// Answer is one scripted answer: a transcript, or an error.
type Answer struct {
	// Text is the transcript.
	Text string
	// Language is the transcript's language; empty means DefaultLanguage.
	Language string
	// Duration is the transcript's duration.
	Duration time.Duration
	// Err, when set, is returned instead of a transcript.
	Err error
}

var _ Transcriber = (*Fake)(nil)

// ErrScriptExhausted is what a Fake answers once its script has run out.
var ErrScriptExhausted = errors.New("whisper: the fake has no scripted answer left")

// NewFake builds a fake that gives the answers in order.
func NewFake(answers ...Answer) *Fake { return &Fake{script: answers} }

// Push appends answers to the script.
func (f *Fake) Push(answers ...Answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, answers...)
}

// Transcribe records the audio and gives the next scripted answer. Like the
// real client, it answers ErrEmptyAudio for empty data, and that call does not
// use up an answer.
func (f *Fake) Transcribe(_ context.Context, audio Audio) (Transcript, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, audio)
	if len(audio.Data) == 0 {
		return Transcript{}, ErrEmptyAudio
	}
	if len(f.script) == 0 {
		return Transcript{}, ErrScriptExhausted
	}
	a := f.script[0]
	f.script = f.script[1:]
	if a.Err != nil {
		return Transcript{}, a.Err
	}
	language := a.Language
	if language == "" {
		language = DefaultLanguage
	}
	return Transcript{Text: a.Text, Language: language, Duration: a.Duration}, nil
}

// Requests are the audio received, in order.
func (f *Fake) Requests() []Audio {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Audio(nil), f.requests...)
}
