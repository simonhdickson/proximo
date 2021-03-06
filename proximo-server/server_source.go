package main

import (
	"context"
	"io"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/uw-labs/proximo/proto"
	"github.com/uw-labs/sync/gogroup"
)

var (
	errStartedTwice   = status.Error(codes.InvalidArgument, "consumption already started")
	errInvalidConfirm = status.Error(codes.InvalidArgument, "invalid confirmation")
	errNotConnected   = status.Error(codes.InvalidArgument, "not connected to a topic")
	errInvalidRequest = status.Error(codes.InvalidArgument, "invalid consumer request - this is possibly a bug in your client library")
)

type consumerConfig struct {
	consumer string
	topic    string
	offset   proto.Offset
}

type consumeHandler interface {
	HandleConsume(ctx context.Context, conf consumerConfig, forClient chan<- *proto.Message, confirmRequest <-chan *proto.Confirmation) error
}

type consumeServer struct {
	handler consumeHandler
}

func (s *consumeServer) Consume(stream proto.MessageSource_ConsumeServer) error {
	sCtx := stream.Context()

	g, ctx := gogroup.New(sCtx)

	forClient := make(chan *proto.Message)
	confirmRequest := make(chan *proto.Confirmation)
	startRequest := make(chan *proto.StartConsumeRequest)

	g.Go(func() error {
		started := false
		for {
			msg, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					return nil
				}
				if strings.HasSuffix(err.Error(), "context canceled") {
					return nil
				}
				return err
			}
			switch {
			case msg.GetStartRequest() != nil:
				if started {
					return errStartedTwice
				}
				started = true
				select {
				case startRequest <- msg.GetStartRequest():
				case <-ctx.Done():
					return nil
				}
			case msg.GetConfirmation() != nil:
				if !started {
					return errInvalidConfirm
				}
				select {
				case confirmRequest <- msg.GetConfirmation():
				case <-ctx.Done():
					return nil
				}
			default:
				return errInvalidRequest
			}
		}
	})

	g.Go(func() error {
		for {
			select {
			case m := <-forClient:
				err := stream.Send(m)
				if err != nil {
					if strings.HasSuffix(err.Error(), "context canceled") {
						return nil
					}
					return err
				}
			case <-ctx.Done():
				return nil
			}
		}
	})

	g.Go(func() error {
		var conf consumerConfig
		select {
		case sr := <-startRequest:
			conf.topic = sr.GetTopic()
			conf.consumer = sr.GetConsumer()
			conf.offset = sr.GetInitialOffset()
		case <-ctx.Done():
			return nil
		}

		return s.handler.HandleConsume(ctx, conf, forClient, confirmRequest)
	})

	if err := g.Wait(); err != nil {
		return err
	}

	if err := sCtx.Err(); err == context.Canceled {
		return status.Error(codes.Canceled, err.Error())
	}
	return sCtx.Err()
}
