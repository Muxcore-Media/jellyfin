package internal

import (
	playbackv1 "github.com/Muxcore-Media/contracts-playback/playbackv1"
)

func streamResolutionFromJFSession(s jfSession) string {
	height, width := 0, 0
	if s.TranscodingInfo != nil && s.TranscodingInfo.Height > 0 {
		height = s.TranscodingInfo.Height
		width = s.TranscodingInfo.Width
	} else if s.NowPlayingItem != nil {
		height = s.NowPlayingItem.Height
		width = s.NowPlayingItem.Width
	}
	return playbackv1.NormalizeStreamResolution(height, width, "")
}

func streamResolutionFromPayload(ev playbackEventPayload) string {
	if ev.StreamResolution != "" {
		return ev.StreamResolution
	}
	return playbackv1.NormalizeStreamResolution(ev.VideoHeight, ev.VideoWidth, "")
}
