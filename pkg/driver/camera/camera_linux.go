package camera

// #include <linux/videodev2.h>
import "C"

import (
	"context"
	"errors"
	"image"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/blackjack/webcam"
	"github.com/pion/mediadevices/pkg/driver"
	"github.com/pion/mediadevices/pkg/frame"
	"github.com/pion/mediadevices/pkg/io/video"
	"github.com/pion/mediadevices/pkg/prop"
)

const (
	maxEmptyFrameCount = 5
	prioritizedDevice  = "video0"
)

var (
	errReadTimeout = errors.New("read timeout")
	errEmptyFrame  = errors.New("empty frame")
	// Reference: https://commons.wikimedia.org/wiki/File:Vector_Video_Standards2.svg
	supportedResolutions = [][2]int{
		{320, 240},
		{640, 480},
		{768, 576},
		{800, 600},
		{1024, 768},
		{1280, 854},
		{1280, 960},
		{1280, 1024},
		{1400, 1050},
		{1600, 1200},
		{2048, 1536},
		{320, 200},
		{800, 480},
		{854, 480},
		{1024, 600},
		{1152, 768},
		{1280, 720},
		{1280, 768},
		{1366, 768},
		{1280, 800},
		{1440, 900},
		{1440, 960},
		{1680, 1050},
		{1920, 1080},
		{2048, 1080},
		{1920, 1200},
		{2560, 1600},
	}
)

// Camera implementation using v4l2
// Reference: https://linuxtv.org/downloads/v4l-dvb-apis/uapi/v4l/videodev.html#videodev
type camera struct {
	path            string
	cam             *webcam.Webcam
	formats         map[webcam.PixelFormat]frame.Format
	reversedFormats map[frame.Format]webcam.PixelFormat
	started         bool
	mutex           sync.Mutex
	cancel          func()
}

func init() {
	discovered := make(map[string]struct{})
	discover(discovered, "/dev/v4l/by-path/*")
	discover(discovered, "/dev/video*")
}

func discover(discovered map[string]struct{}, pattern string) {
	devices, err := filepath.Glob(pattern)
	if err != nil {
		// No v4l device.
		return
	}
	for _, device := range devices {
		label := filepath.Base(device)
		reallink, err := os.Readlink(device)
		if err != nil {
			reallink = label
		} else {
			reallink = filepath.Base(reallink)
		}
		if _, ok := discovered[reallink]; ok {
			continue
		}

		discovered[reallink] = struct{}{}
		cam := newCamera(device)
		priority := driver.PriorityNormal
		if reallink == prioritizedDevice {
			priority = driver.PriorityHigh
		}
		driver.GetManager().Register(cam, driver.Info{
			Label:      label + LabelSeparator + reallink,
			DeviceType: driver.Camera,
			Priority:   priority,
		})
	}
}

func newCamera(path string) *camera {
	formats := map[webcam.PixelFormat]frame.Format{
		webcam.PixelFormat(C.V4L2_PIX_FMT_YUV420): frame.FormatI420,
		webcam.PixelFormat(C.V4L2_PIX_FMT_NV21):   frame.FormatNV21,
		webcam.PixelFormat(C.V4L2_PIX_FMT_NV12):   frame.FormatNV12,
		webcam.PixelFormat(C.V4L2_PIX_FMT_YUYV):   frame.FormatYUYV,
		webcam.PixelFormat(C.V4L2_PIX_FMT_UYVY):   frame.FormatUYVY,
		webcam.PixelFormat(C.V4L2_PIX_FMT_MJPEG):  frame.FormatMJPEG,
		webcam.PixelFormat(C.V4L2_PIX_FMT_Z16):    frame.FormatZ16,
	}

	reversedFormats := make(map[frame.Format]webcam.PixelFormat)
	for k, v := range formats {
		reversedFormats[v] = k
	}

	c := &camera{
		path:            path,
		formats:         formats,
		reversedFormats: reversedFormats,
	}
	return c
}

func (c *camera) log(msg string, args ...interface{}) {
	log.Printf("[Camera %s] "+msg, append([]interface{}{c.path}, args...)...)
}

func getCameraReadTimeout() uint32 {
	// default to 5 seconds
	var readTimeoutSec uint32 = 5
	if val, ok := os.LookupEnv("PION_MEDIADEVICES_CAMERA_READ_TIMEOUT"); ok {
		if valInt, err := strconv.Atoi(val); err == nil {
			if valInt > 0 {
				readTimeoutSec = uint32(valInt)
			}
		}
	}
	return readTimeoutSec
}

func (c *camera) Open() error {
	cam, err := webcam.Open(c.path)
	if err != nil {
		return err
	}
	c.log("Camera open")

	// Late frames should be discarded. Buffering should be handled in higher level.
	cam.SetBufferCount(1)
	c.cam = cam
	return nil
}

func (c *camera) Close() error {
	if c.cam == nil {
		c.log("Closing camera while already closed")
		return nil
	}

	if c.cancel != nil {
		c.log("Canceling reader")
		// Let the reader knows that the caller has closed the camera
		c.cancel()
		// Wait until the reader unref the buffer
		c.mutex.Lock()
		defer c.mutex.Unlock()

		// Note: StopStreaming frees frame buffers even if they are still used in Go code.
		//       There is currently no convenient way to do this safely.
		//       So, consumer of this stream must close camera after unusing all images.
		c.cam.StopStreaming()
		c.cancel = nil
	}
	c.log("Closing camera")
	c.cam.Close()
	return nil
}

func (c *camera) VideoRecord(p prop.Media) (video.Reader, error) {
	c.log("Create new recorder for media: %v", p)
	decoder, err := frame.NewDecoder(p.FrameFormat)
	if err != nil {
		c.log("Error while creating decoder: %s", err)
		return nil, err
	}

	pf := c.reversedFormats[p.FrameFormat]
	c.log("Set FrameFormat and size: %s, %dx%d", p.FrameFormat, p.Width, p.Height)
	actualPF, actualWidth, actualHeight, err := c.cam.SetImageFormat(pf, uint32(p.Width), uint32(p.Height))
	if err != nil {
		c.log("Error while setting image format: %s", err)
		return nil, err
	}
	c.log("Actual image format: %s, %dx%d", actualPF, actualWidth, actualHeight)

	if p.FrameRate > 0 {
		err = c.cam.SetFramerate(float32(p.FrameRate))
		c.log("Frame Rate: %d", p.FrameRate)
		if err != nil {
			return nil, err
		}
	}

	if err := c.cam.StartStreaming(); err != nil {
		c.log("Error while starting streaming: %s", err)
		return nil, err
	}
	c.log("Started streaming")

	cam := c.cam

	readTimeoutSec := getCameraReadTimeout()
	c.log("Read timeout (time to wait for a new frame): %d", readTimeoutSec)

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	var buf []byte
	nbFrames := 0
	framesSince := time.Now()
	r := video.ReaderFunc(func() (img image.Image, release func(), err error) {
		// Lock to avoid accessing the buffer after StopStreaming()
		c.mutex.Lock()
		defer c.mutex.Unlock()

		// Wait until a frame is ready
		for i := 0; i < maxEmptyFrameCount; i++ {
			if ctx.Err() != nil {
				// Return EOF if the camera is already closed.
				return nil, func() {}, io.EOF
			}

			err := cam.WaitForFrame(readTimeoutSec)
			switch err.(type) {
			case nil:
			case *webcam.Timeout:
				c.log("Read timeout, take loo long to receive a frame from camera")
				return nil, func() {}, errReadTimeout
			default:
				c.log("Error while waiting frame: %s", err)
				// Camera has been stopped.
				return nil, func() {}, err
			}

			b, err := cam.ReadFrame()
			if err != nil {
				c.log("Error while reading frame: %s", err)
				// Camera has been stopped.
				return nil, func() {}, err
			}

			// Frame is empty.
			// Retry reading and return errEmptyFrame if it exceeds maxEmptyFrameCount.
			if len(b) == 0 {
				c.log("Frame read but was empty. Only %d consecutive empty frames allowed, %d remaining", maxEmptyFrameCount, maxEmptyFrameCount-i)
				continue
			}

			if len(b) > len(buf) {
				// Grow the intermediate buffer
				buf = make([]byte, len(b))
			}

			// move the memory from mmap to Go. This will guarantee that any data that's going out
			// from this reader will be Go safe. Otherwise, it's possible that outside of this reader
			// that this memory is still being used even after we close it.
			n := copy(buf, b)

			nbFrames++
			if nbFrames == 30*60*2 {
				elapsedTime := time.Now().Sub(framesSince)
				c.log("%d frames have been read in %.3fs (%.1f FPS)", nbFrames, elapsedTime.Seconds(), float64(nbFrames)/elapsedTime.Seconds())
				nbFrames = 0
				framesSince = time.Now()
			}

			img, release, err = decoder.Decode(buf[:n], p.Width, p.Height)
			if errors.Is(err, frame.DecoderError) {
				continue
			}
			return img, release, err
		}
		c.log("Too much consecutive empty undecodable frames")
		return nil, func() {}, errEmptyFrame
	})

	return r, nil
}

func (c *camera) Properties() []prop.Media {
	properties := make([]prop.Media, 0)
	for format := range c.cam.GetSupportedFormats() {
		for _, frameSize := range c.cam.GetSupportedFrameSizes(format) {
			supportedFormat, ok := c.formats[format]
			if !ok {
				continue
			}

			if frameSize.StepWidth == 0 || frameSize.StepHeight == 0 {
				properties = append(properties, prop.Media{
					Video: prop.Video{
						Width:       int(frameSize.MaxWidth),
						Height:      int(frameSize.MaxHeight),
						FrameFormat: supportedFormat,
					},
				})
			} else {
				// FIXME: we should probably use a custom data structure to capture all of the supported resolutions
				for _, supportedResolution := range supportedResolutions {
					minWidth, minHeight := int(frameSize.MinWidth), int(frameSize.MinHeight)
					maxWidth, maxHeight := int(frameSize.MaxWidth), int(frameSize.MaxHeight)
					stepWidth, stepHeight := int(frameSize.StepWidth), int(frameSize.StepHeight)
					width, height := supportedResolution[0], supportedResolution[1]

					if width < minWidth || width > maxWidth ||
						height < minHeight || height > maxHeight {
						continue
					}

					if (width-minWidth)%stepWidth != 0 ||
						(height-minHeight)%stepHeight != 0 {
						continue
					}

					properties = append(properties, prop.Media{
						Video: prop.Video{
							Width:       width,
							Height:      height,
							FrameFormat: supportedFormat,
						},
					})
				}
			}
		}
	}
	return properties
}
