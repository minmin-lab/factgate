package gateway

import (
	"reflect"
	"testing"
)

func TestPreviewPageEndsAndPointsAtTheNextRow(t *testing.T) {
	for _, tc := range []struct {
		offset, returned, rowCount int64
		want                       map[string]any
	}{
		{0, 20, 868, map[string]any{"has_more": true, "next_offset": int64(20)}},
		{800, 68, 868, map[string]any{"has_more": false}},
		{860, 8, 868, map[string]any{"has_more": false}},
		{900, 0, 868, map[string]any{"has_more": false}},
		{0, 0, 0, map[string]any{"has_more": false}},
	} {
		if got := previewPage(tc.offset, tc.returned, tc.rowCount); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("previewPage(%d, %d, %d) = %v, want %v", tc.offset, tc.returned, tc.rowCount, got, tc.want)
		}
	}
}
