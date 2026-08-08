package execution

import "testing"

func TestHasConflictingConversationEntity(t *testing.T) {
	tests := []struct {
		name     string
		question string
		prior    string
		want     bool
	}{
		{
			name:     "email switch",
			question: "看一下 cgs_12@icloud.com 的设备",
			prior:    "分析 tim.you@dreo.com 删除账号失败",
			want:     true,
		},
		{
			name:     "same email",
			question: "继续查 tim.you@dreo.com",
			prior:    "分析 tim.you@dreo.com 删除账号失败",
		},
		{
			name:     "trace switch",
			question: "分析 942a57ca7c3d",
			prior:    "刚才分析的是 42243c84f4f2",
			want:     true,
		},
		{
			name:     "no explicit current entity",
			question: "为什么会这样",
			prior:    "分析 tim.you@dreo.com 删除账号失败",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasConflictingConversationEntity(test.question, test.prior); got != test.want {
				t.Fatalf("hasConflictingConversationEntity() = %v, want %v", got, test.want)
			}
		})
	}
}
