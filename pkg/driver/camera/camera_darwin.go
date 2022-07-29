package camera

import (
	"errors"
	"fmt"
	"github.com/pion/mediadevices/pkg/avfoundation"
	"github.com/pion/mediadevices/pkg/driver"
	"github.com/pion/mediadevices/pkg/frame"
	"github.com/pion/mediadevices/pkg/io/video"
	"github.com/pion/mediadevices/pkg/prop"
	"image"
)

type camera struct {
	device  avfoundation.Device
	session *avfoundation.Session
	rcClose func()
}

var (
	maxDecodeErrors = 3
)

func init() {
	devices, err := avfoundation.Devices(avfoundation.Video)
	if err != nil {
		panic(err)
	}

	for _, device := range devices {
		cam := newCamera(device)
		driver.GetManager().Register(cam, driver.Info{
			Label:      device.UID,
			DeviceType: driver.Camera,
		})
	}
}

func newCamera(device avfoundation.Device) *camera {
	return &camera{
		device: device,
	}
}

func (cam *camera) Open() error {
	var err error
	cam.session, err = avfoundation.NewSession(cam.device)
	return err
}

func (cam *camera) Close() error {
	if cam.rcClose != nil {
		cam.rcClose()
	}
	return cam.session.Close()
}

func (cam *camera) VideoRecord(property prop.Media) (video.Reader, error) {
	decoder, err := frame.NewDecoder(property.FrameFormat)
	if err != nil {
		return nil, err
	}

	rc, err := cam.session.Open(property)
	if err != nil {
		return nil, err
	}
	cam.rcClose = rc.Close
	r := video.ReaderFunc(func() (image.Image, func(), error) {
		var err error
		var frameBuffer []byte
		for i := 0; i < maxDecodeErrors; i++ {
			frameBuffer, _, err = rc.Read()
			if err != nil {
				if errors.Is(err, frame.DecoderError) {
					continue // ignore decoder errors
				}
				return nil, func() {}, err
			}
			return decoder.Decode(frameBuffer, property.Width, property.Height)
		}
		return nil, nil, fmt.Errorf("too many decoder errors: %w", err)
	})
	return r, nil
}

func (cam *camera) Properties() []prop.Media {
	return cam.session.Properties()
}
