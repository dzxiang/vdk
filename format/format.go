package format

import (
	"github.com/dzxiang/vdk/av/avutil"
	"github.com/dzxiang/vdk/format/aac"
	"github.com/dzxiang/vdk/format/flv"
	"github.com/dzxiang/vdk/format/mp4"
	"github.com/dzxiang/vdk/format/rtmp"
	"github.com/dzxiang/vdk/format/rtsp"
	"github.com/dzxiang/vdk/format/ts"
)

func RegisterAll() {
	avutil.DefaultHandlers.Add(mp4.Handler)
	avutil.DefaultHandlers.Add(ts.Handler)
	avutil.DefaultHandlers.Add(rtmp.Handler)
	avutil.DefaultHandlers.Add(rtsp.Handler)
	avutil.DefaultHandlers.Add(flv.Handler)
	avutil.DefaultHandlers.Add(aac.Handler)
}
