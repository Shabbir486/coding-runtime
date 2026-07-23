package unit_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/revature/corems-code-executor/internal/batchproc"
	"github.com/revature/corems-code-executor/internal/models"
)

// --- local mocks (mockBatchRepo is reused from webhook_test.go) --------------

type mockLanguageRepo struct{ mock.Mock }

func (m *mockLanguageRepo) GetAll(ctx context.Context) ([]*models.Language, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*models.Language), args.Error(1)
}
func (m *mockLanguageRepo) GetByID(ctx context.Context, id int) (*models.Language, error) {
	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.Language), args.Error(1)
}
func (m *mockLanguageRepo) GetActive(ctx context.Context) ([]*models.Language, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]*models.Language), args.Error(1)
}

func (m *mockLanguageRepo) SetActive(ctx context.Context, id int, active bool) (*models.Language, error) {
	args := m.Called(ctx, id, active)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*models.Language), args.Error(1)
}

type mockPublisher struct {
	mu        sync.Mutex
	published [][]*models.ExecutionJob
	err       error
}

func (m *mockPublisher) PublishJob(context.Context, *models.ExecutionJob) error { return nil }
func (m *mockPublisher) PublishPriorityJob(context.Context, *models.ExecutionJob) error {
	return nil
}
func (m *mockPublisher) PublishBatch(_ context.Context, jobs []*models.ExecutionJob) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.published = append(m.published, jobs)
	return nil
}
func (m *mockPublisher) Close() {}

type fakeSeeder struct{ count int }

func (s *fakeSeeder) Set(context.Context, string, *models.SubmissionResponse) error {
	s.count++
	return nil
}

func twoSubmissions(batchID string) []*models.Submission {
	b := batchID
	return []*models.Submission{
		{Token: "t1", LanguageID: 1, SourceCode: "x", BatchID: &b},
		{Token: "t2", LanguageID: 1, SourceCode: "y", BatchID: &b},
	}
}

// --- tests -------------------------------------------------------------------

func TestStarter_Start_FansOutWhenPending(t *testing.T) {
	br := new(mockBatchRepo)
	lr := new(mockLanguageRepo)
	pub := &mockPublisher{}
	seeder := &fakeSeeder{}

	br.On("MarkProcessing", mock.Anything, "b1").Return(true, nil)
	br.On("ListSubmissions", mock.Anything, "b1").Return(twoSubmissions("b1"), nil)
	lr.On("GetByID", mock.Anything, 1).Return(&models.Language{ID: 1, Name: "Python"}, nil)

	s := batchproc.NewStarter(br, lr, pub, seeder, zap.NewNop())
	err := s.Start(context.Background(), "b1")

	require.NoError(t, err)
	require.Len(t, pub.published, 1)
	assert.Len(t, pub.published[0], 2) // both submissions fanned out
	assert.Equal(t, 2, seeder.count)
	br.AssertNotCalled(t, "MarkPending", mock.Anything, "b1")
}

func TestStarter_Start_IdempotentWhenNotPending(t *testing.T) {
	br := new(mockBatchRepo)
	pub := &mockPublisher{}

	// Already started/finished → transition claims nothing.
	br.On("MarkProcessing", mock.Anything, "b2").Return(false, nil)

	s := batchproc.NewStarter(br, new(mockLanguageRepo), pub, &fakeSeeder{}, zap.NewNop())
	err := s.Start(context.Background(), "b2")

	assert.ErrorIs(t, err, batchproc.ErrNotPending)
	assert.Empty(t, pub.published) // nothing enqueued
	br.AssertNotCalled(t, "ListSubmissions", mock.Anything, "b2")
}

func TestStarter_Start_RollsBackOnFanOutFailure(t *testing.T) {
	br := new(mockBatchRepo)
	lr := new(mockLanguageRepo)
	pub := &mockPublisher{err: errors.New("sqs down")}

	br.On("MarkProcessing", mock.Anything, "b3").Return(true, nil)
	br.On("ListSubmissions", mock.Anything, "b3").Return(twoSubmissions("b3"), nil)
	lr.On("GetByID", mock.Anything, 1).Return(&models.Language{ID: 1, Name: "Python"}, nil)
	br.On("MarkPending", mock.Anything, "b3").Return(nil)

	s := batchproc.NewStarter(br, lr, pub, &fakeSeeder{}, zap.NewNop())
	err := s.Start(context.Background(), "b3")

	require.Error(t, err)
	// Rolled back to pending so a redelivered START event can retry the fan-out.
	br.AssertCalled(t, "MarkPending", mock.Anything, "b3")
}
