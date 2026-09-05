package processor

import (
	"context"
	"sync"

	"github.com/umputun/ralphex/pkg/executor"
	"github.com/umputun/ralphex/pkg/status"
)

// These package-local mocks keep processor's internal tests from importing the
// generated mocks package, which now necessarily imports processor for the
// ReviewCheckpoint value type.
type testExecutorMock struct {
	RunFunc func(context.Context, string) executor.Result
	mu      sync.Mutex
	calls   []struct {
		Context context.Context
		Prompt  string
	}
}

func (m *testExecutorMock) Run(ctx context.Context, prompt string) executor.Result {
	m.mu.Lock()
	m.calls = append(m.calls, struct {
		Context context.Context
		Prompt  string
	}{ctx, prompt})
	m.mu.Unlock()
	return m.RunFunc(ctx, prompt)
}

func (m *testExecutorMock) RunCalls() []struct {
	Context context.Context
	Prompt  string
} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]struct {
		Context context.Context
		Prompt  string
	}(nil), m.calls...)
}

type testLoggerMock struct {
	PrintFunc          func(string, ...any)
	PrintRawFunc       func(string, ...any)
	PrintSectionFunc   func(status.Section)
	PrintAlignedFunc   func(string)
	LogQuestionFunc    func(string, []string)
	LogAnswerFunc      func(string)
	LogDraftReviewFunc func(string, string)
	PathFunc           func() string
	mu                 sync.Mutex
	printCalls         []struct {
		Format string
		Args   []any
	}
	sectionCalls []struct{ Section status.Section }
}

func (m *testLoggerMock) Print(format string, args ...any) {
	m.mu.Lock()
	m.printCalls = append(m.printCalls, struct {
		Format string
		Args   []any
	}{format, append([]any(nil), args...)})
	m.mu.Unlock()
	m.PrintFunc(format, args...)
}

func (m *testLoggerMock) PrintRaw(format string, args ...any) { m.PrintRawFunc(format, args...) }
func (m *testLoggerMock) PrintSection(section status.Section) {
	m.mu.Lock()
	m.sectionCalls = append(m.sectionCalls, struct{ Section status.Section }{section})
	m.mu.Unlock()
	m.PrintSectionFunc(section)
}
func (m *testLoggerMock) PrintAligned(value string) { m.PrintAlignedFunc(value) }
func (m *testLoggerMock) LogQuestion(question string, options []string) {
	m.LogQuestionFunc(question, options)
}
func (m *testLoggerMock) LogAnswer(answer string) { m.LogAnswerFunc(answer) }
func (m *testLoggerMock) LogDraftReview(action, feedback string) {
	m.LogDraftReviewFunc(action, feedback)
}
func (m *testLoggerMock) Path() string { return m.PathFunc() }

func (m *testLoggerMock) PrintCalls() []struct {
	Format string
	Args   []any
} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]struct {
		Format string
		Args   []any
	}(nil), m.printCalls...)
}

func (m *testLoggerMock) PrintSectionCalls() []struct{ Section status.Section } {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]struct{ Section status.Section }(nil), m.sectionCalls...)
}

type testInputCollectorMock struct {
	AskQuestionFunc    func(context.Context, string, []string) (string, error)
	AskDraftReviewFunc func(context.Context, string, string) (string, string, error)
	mu                 sync.Mutex
	questionCalls      []struct {
		Context  context.Context
		Question string
		Options  []string
	}
}

func (m *testInputCollectorMock) AskQuestion(ctx context.Context, question string, options []string) (string, error) {
	m.mu.Lock()
	m.questionCalls = append(m.questionCalls, struct {
		Context  context.Context
		Question string
		Options  []string
	}{ctx, question, append([]string(nil), options...)})
	m.mu.Unlock()
	return m.AskQuestionFunc(ctx, question, options)
}

func (m *testInputCollectorMock) AskDraftReview(ctx context.Context, question, plan string) (string, string, error) {
	return m.AskDraftReviewFunc(ctx, question, plan)
}

func (m *testInputCollectorMock) AskQuestionCalls() []struct {
	Context  context.Context
	Question string
	Options  []string
} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]struct {
		Context  context.Context
		Question string
		Options  []string
	}(nil), m.questionCalls...)
}
