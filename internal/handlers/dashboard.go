package handlers

import (
	"net/http"
	"sort"
	"time"

	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
)

const (
	dashTrendDays   = 7
	dashRecentLimit = 10
)

// dashChannel 仪表盘统计的一个注册渠道：每个渠道独立建表、各有一套生产器。
type dashChannel struct {
	key, name, page string
	model           any
	progress        func() dashProgress
}

type dashProgress struct {
	running    bool
	runningNum int
	pending    int
	// 只有 GPT 生产器按「本次目标」计数，其余渠道为零值。
	target  int
	done    int
	message string
}

type dashChannelStats struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	Page       string `json:"page"`
	Total      int64  `json:"total"`
	Registered int64  `json:"registered"`
	InProgress int64  `json:"in_progress"` // 待生产 + 注册中 + 等待验证码
	Failed     int64  `json:"failed"`
	Unshipped  int64  `json:"unshipped"` // 已注册未出库，即可出库库存
	Mother     int64  `json:"mother,omitempty"`
	Fission    int64  `json:"fission,omitempty"`
	Producing  bool   `json:"producing"`
	RunningNum int    `json:"running_num"`
	Pending    int    `json:"pending"`
	Target     int    `json:"target,omitempty"`
	RunDone    int    `json:"run_registered,omitempty"`
	Message    string `json:"message,omitempty"`
}

type dashRecent struct {
	Channel   string    `json:"channel"`
	Email     string    `json:"email"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *Handler) dashChannels() []dashChannel {
	return []dashChannel{
		{"chatgpt", "GPT", "/accounts", &models.Registration{}, func() dashProgress {
			p := h.Producer.Snapshot()
			return dashProgress{p.Running, p.RunningNum, p.Pending, p.Target, p.Registered, p.Message}
		}},
		{"grok", "Grok", "/grok", &models.GrokRegistration{}, func() dashProgress {
			p := h.GrokProducer.Snapshot()
			return dashProgress{running: p.Running, runningNum: p.RunningNum, pending: p.Pending}
		}},
		{"adobe", "Adobe", "/adobe", &models.AdobeRegistration{}, func() dashProgress {
			p := h.AdobeProducer.Snapshot()
			return dashProgress{running: p.Running, runningNum: p.RunningNum, pending: p.Pending}
		}},
		{"leonardo", "Leonardo", "/leonardo", &models.LeonardoRegistration{}, func() dashProgress {
			p := h.LeonardoProducer.Snapshot()
			return dashProgress{running: p.Running, runningNum: p.RunningNum, pending: p.Pending}
		}},
		{"lovart", "Lovart", "/lovart", &models.LovartRegistration{}, func() dashProgress {
			p := h.LovartProducer.Snapshot()
			return dashProgress{running: p.Running, runningNum: p.RunningNum, pending: p.Pending}
		}},
	}
}

func (h *Handler) dashChannelStats(ch dashChannel) dashChannelStats {
	st := dashChannelStats{Key: ch.key, Name: ch.name, Page: ch.page}
	var rows []struct {
		Status string
		N      int64
	}
	h.DB.Model(ch.model).Select("status, count(*) AS n").Group("status").Scan(&rows)
	for _, r := range rows {
		st.Total += r.N
		switch r.Status {
		case "registered":
			st.Registered = r.N
		case "pending", "registering", "waiting_code":
			st.InProgress += r.N
		case "register_failed":
			st.Failed = r.N
		}
	}
	var shipped int64
	h.DB.Model(ch.model).Where("status = ? AND shipped = ?", "registered", true).Count(&shipped)
	st.Unshipped = st.Registered - shipped
	if ch.key == "chatgpt" {
		h.DB.Model(ch.model).Where("status = ? AND is_mother = ?", "registered", true).Count(&st.Mother)
		st.Fission = st.Registered - st.Mother
	}
	p := ch.progress()
	st.Producing, st.RunningNum, st.Pending = p.running, p.runningNum, p.pending
	st.Target, st.RunDone, st.Message = p.target, p.done, p.message
	return st
}

// Stats 仪表盘数据：全渠道汇总、各渠道概览、近 7 天按渠道产量、最近注册。
func (h *Handler) Stats(c *gin.Context) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	days := make([]string, dashTrendDays)
	trend := make(map[string]map[string]int64, dashTrendDays)
	for i := range days {
		days[i] = today.AddDate(0, 0, i-dashTrendDays+1).Format("2006-01-02")
		trend[days[i]] = map[string]int64{}
	}

	channels := []dashChannelStats{}
	recent := []dashRecent{}
	var registered, failed, unshipped int64
	running := 0
	for _, ch := range h.dashChannels() {
		st := h.dashChannelStats(ch)
		channels = append(channels, st)
		registered += st.Registered
		failed += st.Failed
		unshipped += st.Unshipped
		running += st.RunningNum

		// created_at 按本地时间带时区存成文本，前 10 位就是本地日期；
		// 用 strftime 会先换算成 UTC，凌晨 0~8 点注册的号会被算到前一天。
		var perDay []struct {
			D string
			N int64
		}
		h.DB.Model(ch.model).Select("substr(created_at, 1, 10) AS d, count(*) AS n").
			Where("status = ? AND created_at >= ?", "registered", days[0]).
			Group("d").Scan(&perDay)
		for _, r := range perDay {
			if m, ok := trend[r.D]; ok {
				m[ch.key] = r.N
			}
		}

		var latest []dashRecent
		h.DB.Model(ch.model).Select("email, status, created_at").
			Order("created_at DESC").Limit(dashRecentLimit).Scan(&latest)
		for i := range latest {
			latest[i].Channel = ch.key
		}
		recent = append(recent, latest...)
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].CreatedAt.After(recent[j].CreatedAt) })
	if len(recent) > dashRecentLimit {
		recent = recent[:dashRecentLimit]
	}

	trendOut := make([]gin.H, 0, len(days))
	for _, d := range days {
		trendOut = append(trendOut, gin.H{"date": d, "counts": trend[d]})
	}
	var todayCount int64
	for _, n := range trend[days[len(days)-1]] {
		todayCount += n
	}

	var mailboxes, mailboxVerified int64
	h.DB.Model(&models.Mailbox{}).Count(&mailboxes)
	h.DB.Model(&models.Mailbox{}).Where("status = ?", "verified").Count(&mailboxVerified)

	c.JSON(http.StatusOK, gin.H{
		"summary": gin.H{
			"registered": registered, "today": todayCount, "running": running,
			"failed": failed, "unshipped": unshipped,
			"mailboxes": mailboxes, "mailbox_verified": mailboxVerified,
		},
		"channels": channels,
		"trend":    trendOut,
		"recent":   recent,
	})
}
