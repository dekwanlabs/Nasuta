package memory

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestTermOverlapRatio(t *testing.T) {
	cases := []struct {
		left, right []string
		want        float64
	}{
		{nil, nil, 0},
		{[]string{"a"}, nil, 0},
		{[]string{"a", "b"}, []string{"a", "b"}, 1},
		{[]string{"a", "b", "c"}, []string{"a"}, 1.0 / 3},
		{[]string{"a", "b"}, []string{"c", "d"}, 0},
		// duplicate terms on the right count once.
		{[]string{"a", "b", "c", "d"}, []string{"a", "a", "b"}, 0.5},
	}
	for _, tc := range cases {
		if got := termOverlapRatio(tc.left, tc.right); got != tc.want {
			t.Errorf("termOverlapRatio(%v, %v) = %v, want %v", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestHasCrossSessionTopic(t *testing.T) {
	memory, _, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()

	// A historical turn in another session shares the 消息中心 bigrams with the
	// current question, so the topic is recurring.
	mock.ExpectQuery(`(?s)SELECT t.question_terms_json, t.entities_json FROM qa_turns t JOIN qa_sessions s`).
		WithArgs(int64(42), "current-session", crossSessionTopicScanLimit).
		WillReturnRows(sqlmock.NewRows([]string{"question_terms_json", "entities_json"}).
			AddRow(`["细说","说一","一下","下消","消息","息中","中心","心的","的全","全部","部业","业务"]`, `[]`))

	recurring, err := memory.HasCrossSessionTopic(
		context.Background(), 42, "current-session",
		"帮我分析一下rgb灯效、消息中心、菜谱、tts这几个业务的流程是什么样的",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !recurring {
		t.Fatal("expected a recurring topic to be detected across sessions")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestHasCrossSessionTopicRejectsOneOffLookup(t *testing.T) {
	memory, _, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()

	// The only historical turn is unrelated (a one-off word lookup), so the
	// current question is not a recurring topic.
	mock.ExpectQuery(`(?s)SELECT t.question_terms_json, t.entities_json FROM qa_turns t JOIN qa_sessions s`).
		WithArgs(int64(42), "current-session", crossSessionTopicScanLimit).
		WillReturnRows(sqlmock.NewRows([]string{"question_terms_json", "entities_json"}).
			AddRow(`["巡航","行的","的单","单词","词是","是啥","啥"]`, `[]`))

	recurring, err := memory.HasCrossSessionTopic(
		context.Background(), 42, "current-session",
		"帮我分析一下rgb灯效、消息中心、菜谱、tts这几个业务的流程是什么样的",
	)
	if err != nil {
		t.Fatal(err)
	}
	if recurring {
		t.Fatal("expected an unrelated historical turn to not count as the same topic")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestHasCrossSessionTopicEmptyHistory(t *testing.T) {
	memory, _, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()

	mock.ExpectQuery(`(?s)SELECT t.question_terms_json, t.entities_json FROM qa_turns t JOIN qa_sessions s`).
		WithArgs(int64(42), "current-session", crossSessionTopicScanLimit).
		WillReturnRows(sqlmock.NewRows([]string{"question_terms_json", "entities_json"}))

	recurring, err := memory.HasCrossSessionTopic(
		context.Background(), 42, "current-session", "帮我分析一下rgb灯效",
	)
	if err != nil {
		t.Fatal(err)
	}
	if recurring {
		t.Fatal("expected no recurring topic when there is no other-session history")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
