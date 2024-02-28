# Don't use this

This is for testing / learning

Modified fork of https://github.com/pion/rtwatch combined with code from https://github.com:bluenviron/mediamtx.git

## Description
Hosts a WebRTC server on port 8080.

Connects to an RTSP server hosting what is assumed to be a h264 video stream.

Transforms the RSTP RTP packets be WebRTC compliant & forwards them to all WebRTC peers connected to the the WebRTC server.

## Usage:
```bash
# In a terminal session:
go run main.go rtsp://localhost:8554/live

# go to http://localhost:8080/ in a browser (tested on chrome) & start the video
# observe that the browser plays the video
```
