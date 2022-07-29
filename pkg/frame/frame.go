package frame

import (
	"errors"
	"fmt"
	"image"
)

type Decoder interface {
	Decode(frame []byte, width, height int) (image.Image, func(), error)
}

// DecoderFunc is a proxy type for Decoder
type decoderFunc func(frame []byte, width, height int) (image.Image, func(), error)

func (f decoderFunc) Decode(frame []byte, width, height int) (image.Image, func(), error) {
	img, release, err := f(frame, width, height)
	if err != nil {
		err = &decoderError{cause: err}
	}
	return img, release, err
}

var DecoderError = errors.New("decoder error")

type decoderError struct {
	cause error
}

func (err *decoderError) Error() string {
	return fmt.Sprintf("%s: %s", DecoderError, err.cause)
}

func (err *decoderError) Cause() error {
	return err.cause
}

func (err *decoderError) Unwrap() error {
	return err.cause
}

func (err *decoderError) Is(target error) bool {
	return errors.Is(target, DecoderError)
}

func (err *decoderError) String() string {
	return err.Error()
}
