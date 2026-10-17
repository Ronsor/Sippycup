package libsippycup_test

import (
	"context"
	"fmt"
	"time"

	"github.com/ronsor/sippycup/libsippycup"
)

func ExampleEngine_Accept() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	echo := func(_ context.Context, request libsippycup.EndpointRequest) (libsippycup.Leg, error) {
		engineLeg, appLeg, err := libsippycup.NewLegPair(request.CallContext, request.Format, 10)
		if err != nil {
			return nil, err
		}

		go func() {
			defer appLeg.Close()
			if err := appLeg.WriteState(request.CallContext, libsippycup.StateAnswered); err != nil {
				return
			}

			for {
				frame, err := appLeg.Read(request.CallContext)
				if err != nil {
					return
				}

				if err := appLeg.Write(request.CallContext, frame); err != nil {
					return
				}
			}
		}()

		return engineLeg, nil
	}

	config := &libsippycup.Config{
		Sources: []*libsippycup.Node{{Name: "app", Type: "inproc"}},
		Endpoints: []*libsippycup.Node{{
			Name: "echo", Type: "inproc", InProcess: echo,
		}},
		Routes: []*libsippycup.Route{{
			Name: "echo", Connect: &libsippycup.Connect{Endpoint: "echo"},
		}},
	}
	if err := config.Validate(); err != nil {
		panic(err)
	}

	engine := libsippycup.NewEngine(config)
	if err := engine.Start(); err != nil {
		panic(err)
	}
	defer engine.Close()

	format := libsippycup.Format{Codec: "pcm_s16le", Rate: 8000, Channels: 1}
	sourceLeg, appLeg, err := libsippycup.NewLegPair(ctx, format, 10)
	if err != nil {
		panic(err)
	}
	defer appLeg.Close()

	done := make(chan error, 1)
	go func() {
		done <- engine.Accept(ctx, "app", libsippycup.Request{}, sourceLeg)
	}()

	if _, err := appLeg.ReadState(ctx); err != nil {
		panic(err)
	}
	if err := appLeg.Write(ctx, libsippycup.Frame{Digit: '5'}); err != nil {
		panic(err)
	}

	frame, err := appLeg.Read(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Received digit %c\n", frame.Digit)

	appLeg.Close()
	<-done
	// Output: Received digit 5
}
