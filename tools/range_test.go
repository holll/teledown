package tools

import (
	"testing"
	"time"
)

func TestParseRange(t *testing.T) {
	t.Run("全部", func(t *testing.T) {
		for _, in := range []string{"", ">0"} {
			r, err := ParseRange(in)
			if err != nil {
				t.Fatalf("ParseRange(%q) 报错: %v", in, err)
			}
			if r.Mode != RangeAll {
				t.Errorf("ParseRange(%q) 应为全部模式，得到 %v", in, r.Mode)
			}
		}
	})

	t.Run("大于", func(t *testing.T) {
		r, err := ParseRange(">100")
		if err != nil {
			t.Fatal(err)
		}
		if r.Mode != RangeMin || r.Min != 100 {
			t.Errorf(">100 解析为 %+v", r)
		}
	})

	t.Run("小于", func(t *testing.T) {
		r, err := ParseRange("<100")
		if err != nil {
			t.Fatal(err)
		}
		if r.Mode != RangeMax || r.Max != 100 {
			t.Errorf("<100 解析为 %+v", r)
		}
	})

	t.Run("单条", func(t *testing.T) {
		for _, in := range []string{"100", "=100"} {
			r, err := ParseRange(in)
			if err != nil {
				t.Fatalf("ParseRange(%q) 报错: %v", in, err)
			}
			if r.Mode != RangeSingle || len(r.IDs) != 1 || r.IDs[0] != 100 {
				t.Errorf("%q 解析为 %+v，期望单条消息 100", in, r)
			}
		}
	})

	t.Run("多条", func(t *testing.T) {
		r, err := ParseRange("1,2,3")
		if err != nil {
			t.Fatal(err)
		}
		if r.Mode != RangeList || len(r.IDs) != 3 {
			t.Errorf("1,2,3 解析为 %+v", r)
		}
	})

	t.Run("区间两种写法等价", func(t *testing.T) {
		a, err := ParseRange("10..200")
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseRange("s10s200")
		if err != nil {
			t.Fatal(err)
		}
		if a.Mode != b.Mode || a.Min != b.Min || a.Max != b.Max {
			t.Errorf("10..200 与 s10s200 应等价: %+v vs %+v", a, b)
		}
		if a.Mode != RangeBetween || a.Min != 10 || a.Max != 200 {
			t.Errorf("区间解析为 %+v", a)
		}
	})

	t.Run("非法输入返回错误", func(t *testing.T) {
		for _, in := range []string{">abc", "sX", "1,abc", "s10", "abc"} {
			if _, err := ParseRange(in); err == nil {
				t.Errorf("ParseRange(%q) 应报错", in)
			}
		}
	})
}

func TestParseDateRange(t *testing.T) {
	at := func(y int, mo time.Month, d, h, mi, s int) int {
		return int(time.Date(y, mo, d, h, mi, s, 0, time.Local).Unix())
	}

	t.Run("未指定时两侧不限", func(t *testing.T) {
		r, err := ParseDateRange("", "")
		if err != nil {
			t.Fatal(err)
		}
		if r.Since != 0 || r.Until != 0 {
			t.Errorf("空参数应两侧不限，得到 %+v", r)
		}
	})

	t.Run("只给日期时含当天整天", func(t *testing.T) {
		r, err := ParseDateRange("2026-09-01", "2026-09-30")
		if err != nil {
			t.Fatal(err)
		}
		if want := at(2026, 9, 1, 0, 0, 0); r.Since != want {
			t.Errorf("Since = %d，期望 %d", r.Since, want)
		}
		if want := at(2026, 9, 30, 23, 59, 59); r.Until != want {
			t.Errorf("Until = %d，期望 %d", r.Until, want)
		}
		// 边界时刻本身要算在内，越界一秒则排除
		for _, ts := range []int{r.Since, r.Until} {
			if !r.Contains(ts) {
				t.Errorf("边界时刻 %d 应被包含", ts)
			}
		}
		for _, ts := range []int{r.Since - 1, r.Until + 1} {
			if r.Contains(ts) {
				t.Errorf("边界外的 %d 应被排除", ts)
			}
		}
	})

	t.Run("带时刻的写法", func(t *testing.T) {
		for _, in := range []string{"2026-09-01 12:30", "2026-09-01T12:30"} {
			r, err := ParseDateRange(in, "")
			if err != nil {
				t.Fatalf("ParseDateRange(%q) 报错: %v", in, err)
			}
			if want := at(2026, 9, 1, 12, 30, 0); r.Since != want {
				t.Errorf("%q 的 Since = %d，期望 %d", in, r.Since, want)
			}
		}
	})

	t.Run("斜杠与秒级写法", func(t *testing.T) {
		r, err := ParseDateRange("", "2026/09/01 12:30:45")
		if err != nil {
			t.Fatal(err)
		}
		if want := at(2026, 9, 1, 12, 30, 45); r.Until != want {
			t.Errorf("Until = %d，期望 %d", r.Until, want)
		}
	})

	t.Run("只有单侧时另一侧不限", func(t *testing.T) {
		r, err := ParseDateRange("2026-09-01", "")
		if err != nil {
			t.Fatal(err)
		}
		if r.Since == 0 || r.Until != 0 {
			t.Errorf("只给 since 时 Until 应为 0，得到 %+v", r)
		}
	})

	t.Run("区间颠倒报错", func(t *testing.T) {
		if _, err := ParseDateRange("2026-10-01", "2026-09-01"); err == nil {
			t.Error("since 晚于 until 应报错")
		}
	})

	t.Run("非法输入报错", func(t *testing.T) {
		for _, in := range []string{"2026-13-01", "2026/9", "昨天", "20260901"} {
			if _, err := ParseDateRange(in, ""); err == nil {
				t.Errorf("ParseDateRange(%q) 应报错", in)
			}
		}
	})
}
