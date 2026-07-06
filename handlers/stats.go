package handlers

import (
	"database/sql"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

// HeatmapDataPoint 热力图单日数据
type HeatmapDataPoint struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// StatsHeatmapHandler 热力图数据 API
// GET /api/stats/heatmap?type=all|posts|moments
func StatsHeatmapHandler(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "内部错误"})
			}
		}()

		typ := c.DefaultQuery("type", "all")
		since := time.Now().AddDate(-1, 0, 0).Format("2006-01-02")

		if typ == "all" {
			posts, err1 := queryDailyCounts(db,
				"SELECT DATE(created_at), COUNT(*) FROM posts WHERE status='published' AND privacy='public' AND DATE(created_at) >= ? GROUP BY DATE(created_at)", since)
			moments, err2 := queryDailyCounts(db,
				"SELECT DATE(created_at), COUNT(*) FROM moments WHERE DATE(created_at) >= ? GROUP BY DATE(created_at)", since)

			if err1 != nil && err2 != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "查询失败"})
				return
			}
			if posts == nil {
				posts = []HeatmapDataPoint{}
			}
			if moments == nil {
				moments = []HeatmapDataPoint{}
			}
			result := mergeMaps(posts, moments)
			if result == nil {
				result = []HeatmapDataPoint{}
			}
			c.JSON(http.StatusOK, gin.H{"ok": true, "data": result})
			return
		}

		var sqlStr string
		if typ == "posts" {
			sqlStr = "SELECT DATE(created_at), COUNT(*) FROM posts WHERE status='published' AND privacy='public' AND DATE(created_at) >= ? GROUP BY DATE(created_at)"
		} else {
			sqlStr = "SELECT DATE(created_at), COUNT(*) FROM moments WHERE DATE(created_at) >= ? GROUP BY DATE(created_at)"
		}

		result, err := queryDailyCounts(db, sqlStr, since)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"ok": false, "msg": "查询失败"})
			return
		}
		if result == nil {
			result = []HeatmapDataPoint{}
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "data": result})
	}
}

// queryDailyCounts 查询每日计数
func queryDailyCounts(db *sql.DB, sqlStr string, args ...interface{}) ([]HeatmapDataPoint, error) {
	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []HeatmapDataPoint
	for rows.Next() {
		var dp HeatmapDataPoint
		if err := rows.Scan(&dp.Date, &dp.Count); err != nil {
			return nil, err
		}
		result = append(result, dp)
	}
	return result, rows.Err()
}

// mergeMaps 合并两个按日期排序的 HeatmapDataPoint 切片
func mergeMaps(a, b []HeatmapDataPoint) []HeatmapDataPoint {
	merged := make(map[string]int)
	for _, v := range a {
		merged[v.Date] += v.Count
	}
	for _, v := range b {
		merged[v.Date] += v.Count
	}
	if len(merged) == 0 {
		return []HeatmapDataPoint{}
	}

	result := make([]HeatmapDataPoint, 0, len(merged))
	for date, count := range merged {
		result = append(result, HeatmapDataPoint{Date: date, Count: count})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Date < result[j].Date
	})
	return result
}
