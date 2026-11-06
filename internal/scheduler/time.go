package scheduler

import "time"

type civilDate struct {
	year  int
	month time.Month
	day   int
}

func dateOf(at time.Time) civilDate {
	year, month, day := at.Date()
	return civilDate{year: year, month: month, day: day}
}

func (d civilDate) utc() time.Time { return time.Date(d.year, d.month, d.day, 0, 0, 0, 0, time.UTC) }
func (d civilDate) add(days int) civilDate {
	return dateOf(d.utc().AddDate(0, 0, days))
}
func (d civilDate) before(other civilDate) bool { return d.utc().Before(other.utc()) }

type scheduleState struct {
	schedule Schedule
	location *time.Location
	hour     int
	minute   int
	consumed civilDate
	next     time.Time
}

// firstWallTime逐个真实时区区间反算UTC，不能依赖time.Date对DST歧义的任意选择。
// 两侧48小时覆盖IANA历史上的整日跳变与非整小时偏移；枚举的是时区转换区间，
// 不是每分钟轮询。不存在的墙钟没有候选；重复墙钟统一取最早的真实时刻。
func (s *scheduleState) firstWallTime(date civilDate) (time.Time, bool) {
	nominal := time.Date(date.year, date.month, date.day, s.hour, s.minute, 0, 0, time.UTC)
	limit := nominal.Add(48 * time.Hour)
	var first time.Time
	consider := func(offset int) {
		candidate := nominal.Add(-time.Duration(offset) * time.Second)
		wall := candidate.In(s.location)
		if dateOf(wall) == date && wall.Hour() == s.hour && wall.Minute() == s.minute &&
			wall.Second() == 0 && (first.IsZero() || candidate.Before(first)) {
			first = candidate
		}
	}
	// time.Date至少提供一个邻近区间的偏移；最终仍必须完整往返校验，不能接受
	// 它对缺失时刻的自动归一化。该候选也覆盖远期TZ规则的人工年末区间边界。
	_, guessedOffset := time.Date(date.year, date.month, date.day, s.hour, s.minute, 0, 0, s.location).Zone()
	consider(guessedOffset)
	for cursor := nominal.Add(-48 * time.Hour); cursor.Before(limit); {
		local := cursor.In(s.location)
		_, offset := local.Zone()
		_, end := local.ZoneBounds()
		consider(offset)
		if end.IsZero() || !end.After(cursor) {
			break
		}
		cursor = end
	}
	return first, !first.IsZero()
}

func (s *scheduleState) nextAfter(now time.Time) time.Time {
	date := dateOf(now.In(s.location))
	if !s.consumed.before(date) {
		date = s.consumed.add(1)
	}
	for ; ; date = date.add(1) {
		if candidate, exists := s.firstWallTime(date); exists && candidate.After(now) {
			return candidate
		}
	}
}

// rebase直接定位现在之后的正常日程，并将此前本地日期整体消费。即使休眠数年，
// 也不逐日生成或回放历史事件；水位只增不减，校时倒退或重新启用不能复活旧日期。
func (s *scheduleState) rebase(now time.Time) {
	if !s.schedule.Enabled {
		s.next = time.Time{}
		return
	}
	s.next = s.nextAfter(now)
	consumed := dateOf(s.next.In(s.location)).add(-1)
	if s.consumed.before(consumed) {
		s.consumed = consumed
	}
}
