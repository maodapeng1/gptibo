package fulfillment

import (
	"strconv"
	"time"
)

func stringTime() string                { return strconv.FormatInt(time.Now().UnixMilli(), 10) }
func parseTime(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
