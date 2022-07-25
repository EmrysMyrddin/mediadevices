package frame

import (
	"bytes"
	"image"
	"image/jpeg"
	"log"
)

func decodeMJPEG(frame []byte, width, height int) (image.Image, func(), error) {
	img, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		log.Printf("[MJPEG] Error while decoding frame: %s.\nBytes: %v", err, frame)
	}
	return img, func() {}, err
}
