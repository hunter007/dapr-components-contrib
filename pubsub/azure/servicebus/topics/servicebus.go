/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package topics

import (
	"context"
	"errors"
	"time"

	servicebus "github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
	"github.com/cenkalti/backoff/v4"

	impl "github.com/dapr/components-contrib/internal/component/azure/servicebus"
	"github.com/dapr/components-contrib/internal/utils"
	contribMetadata "github.com/dapr/components-contrib/metadata"
	"github.com/dapr/components-contrib/pubsub"
	"github.com/dapr/kit/logger"
	"github.com/dapr/kit/retry"
)

const (
	requireSessionsMetadataKey       = "requireSessions"
	sessionIdleTimeoutMetadataKey    = "sessionIdleTimeout"
	maxConcurrentSessionsMetadataKey = "maxConcurrentSessions"

	defaultMaxBulkSubCount                 = 100
	defaultMaxBulkPubBytes          uint64 = 1024 * 128 // 128 KiB
	defaultSesssionIdleTimeoutInSec        = 60
	defaultMaxConcurrentSessions           = 8
)

type azureServiceBus struct {
	metadata      *impl.Metadata
	client        *impl.Client
	logger        logger.Logger
	features      []pubsub.Feature
	publishCtx    context.Context
	publishCancel context.CancelFunc
}

// NewAzureServiceBusTopics returns a new pub-sub implementation.
func NewAzureServiceBusTopics(logger logger.Logger) pubsub.PubSub {
	return &azureServiceBus{
		logger:   logger,
		features: []pubsub.Feature{pubsub.FeatureMessageTTL},
	}
}

func (a *azureServiceBus) Init(metadata pubsub.Metadata) (err error) {
	a.metadata, err = impl.ParseMetadata(metadata.Properties, a.logger, impl.MetadataModeTopics)
	if err != nil {
		return err
	}

	a.client, err = impl.NewClient(a.metadata, metadata.Properties)
	if err != nil {
		return err
	}

	a.publishCtx, a.publishCancel = context.WithCancel(context.Background())

	return nil
}

func (a *azureServiceBus) Publish(req *pubsub.PublishRequest) error {
	msg, err := impl.NewASBMessageFromPubsubRequest(req)
	if err != nil {
		return err
	}

	ebo := backoff.NewExponentialBackOff()
	ebo.InitialInterval = time.Duration(a.metadata.PublishInitialRetryIntervalInMs) * time.Millisecond
	bo := backoff.WithMaxRetries(ebo, uint64(a.metadata.PublishMaxRetries))
	bo = backoff.WithContext(bo, a.publishCtx)

	msgID := "nil"
	if msg.MessageID != nil {
		msgID = *msg.MessageID
	}
	return retry.NotifyRecover(
		func() (err error) {
			// Ensure the queue or topic exists the first time it is referenced
			// This does nothing if DisableEntityManagement is true
			err = a.client.EnsureTopic(a.publishCtx, req.Topic)
			if err != nil {
				return err
			}

			// Get the sender
			var sender *servicebus.Sender
			sender, err = a.client.GetSender(a.publishCtx, req.Topic)
			if err != nil {
				return err
			}

			// Try sending the message
			ctx, cancel := context.WithTimeout(a.publishCtx, time.Second*time.Duration(a.metadata.TimeoutInSec))
			defer cancel()
			err = sender.SendMessage(ctx, msg, nil)
			if err != nil {
				if impl.IsNetworkError(err) {
					// Retry after reconnecting
					a.client.CloseSender(req.Topic)
					return err
				}

				if impl.IsRetriableAMQPError(err) {
					// Retry (no need to reconnect)
					return err
				}

				// Do not retry on other errors
				return backoff.Permanent(err)
			}
			return nil
		},
		bo,
		func(err error, _ time.Duration) {
			a.logger.Warnf("Could not publish service bus message (%s). Retrying...: %v", msgID, err)
		},
		func() {
			a.logger.Infof("Successfully published service bus message (%s) after it previously failed", msgID)
		},
	)
}

func (a *azureServiceBus) BulkPublish(ctx context.Context, req *pubsub.BulkPublishRequest) (pubsub.BulkPublishResponse, error) {
	// If the request is empty, sender.SendMessageBatch will panic later.
	// Return an empty response to avoid this.
	if len(req.Entries) == 0 {
		a.logger.Warnf("Empty bulk publish request, skipping")
		return pubsub.NewBulkPublishResponse(req.Entries, pubsub.PublishSucceeded, nil), nil
	}

	// Ensure the queue or topic exists the first time it is referenced
	// This does nothing if DisableEntityManagement is true
	err := a.client.EnsureTopic(a.publishCtx, req.Topic)
	if err != nil {
		return pubsub.NewBulkPublishResponse(req.Entries, pubsub.PublishFailed, err), err
	}

	// Get the sender
	sender, err := a.client.GetSender(ctx, req.Topic)
	if err != nil {
		return pubsub.NewBulkPublishResponse(req.Entries, pubsub.PublishFailed, err), err
	}

	// Create a new batch of messages with batch options.
	batchOpts := &servicebus.MessageBatchOptions{
		MaxBytes: utils.GetElemOrDefaultFromMap(req.Metadata, contribMetadata.MaxBulkPubBytesKey, defaultMaxBulkPubBytes),
	}

	batchMsg, err := sender.NewMessageBatch(ctx, batchOpts)
	if err != nil {
		return pubsub.NewBulkPublishResponse(req.Entries, pubsub.PublishFailed, err), err
	}

	// Add messages from the bulk publish request to the batch.
	err = impl.UpdateASBBatchMessageWithBulkPublishRequest(batchMsg, req)
	if err != nil {
		return pubsub.NewBulkPublishResponse(req.Entries, pubsub.PublishFailed, err), err
	}

	// Azure Service Bus does not return individual status for each message in the request.
	err = sender.SendMessageBatch(ctx, batchMsg, nil)
	if err != nil {
		return pubsub.NewBulkPublishResponse(req.Entries, pubsub.PublishFailed, err), err
	}

	return pubsub.NewBulkPublishResponse(req.Entries, pubsub.PublishSucceeded, nil), nil
}

func (a *azureServiceBus) Subscribe(subscribeCtx context.Context, req pubsub.SubscribeRequest, handler pubsub.Handler) error {
	var requireSessions bool
	if val, ok := req.Metadata[requireSessionsMetadataKey]; ok && val != "" {
		requireSessions = utils.IsTruthy(val)
	}
	sessionIdleTimeout := time.Duration(utils.GetElemOrDefaultFromMap(req.Metadata, sessionIdleTimeoutMetadataKey, defaultSesssionIdleTimeoutInSec)) * time.Second
	maxConcurrentSessions := utils.GetElemOrDefaultFromMap(req.Metadata, maxConcurrentSessionsMetadataKey, defaultMaxConcurrentSessions)

	sub := impl.NewSubscription(
		subscribeCtx,
		a.metadata.MaxActiveMessages,
		a.metadata.TimeoutInSec,
		nil,
		a.metadata.MaxRetriableErrorsPerSec,
		a.metadata.MaxConcurrentHandlers,
		"topic "+req.Topic,
		a.metadata.LockRenewalInSec,
		requireSessions,
		a.logger,
	)

	receiveAndBlockFn := func(receiver impl.Receiver, onFirstSuccess func()) error {
		return sub.ReceiveBlocking(
			impl.GetPubSubHandlerFunc(req.Topic, handler, a.logger, time.Duration(a.metadata.HandlerTimeoutInSec)*time.Second),
			receiver,
			onFirstSuccess,
			impl.ReceiveOptions{
				BulkEnabled:        false, // Bulk is not supported in regular Subscribe.
				SessionIdleTimeout: sessionIdleTimeout,
			},
		)
	}

	return a.doSubscribe(subscribeCtx, req, sub, receiveAndBlockFn, impl.SubscriptionOpts{
		RequireSessions:      requireSessions,
		MaxConcurrentSesions: maxConcurrentSessions,
	})
}

func (a *azureServiceBus) BulkSubscribe(subscribeCtx context.Context, req pubsub.SubscribeRequest, handler pubsub.BulkHandler) error {
	var requireSessions bool
	if val, ok := req.Metadata[requireSessionsMetadataKey]; ok && val != "" {
		requireSessions = utils.IsTruthy(val)
	}
	sessionIdleTimeout := time.Duration(utils.GetElemOrDefaultFromMap(req.Metadata, sessionIdleTimeoutMetadataKey, defaultSesssionIdleTimeoutInSec)) * time.Second
	maxConcurrentSessions := utils.GetElemOrDefaultFromMap(req.Metadata, maxConcurrentSessionsMetadataKey, defaultMaxConcurrentSessions)

	maxBulkSubCount := utils.GetElemOrDefaultFromMap(req.Metadata, contribMetadata.MaxBulkSubCountKey, defaultMaxBulkSubCount)
	sub := impl.NewSubscription(
		subscribeCtx,
		a.metadata.MaxActiveMessages,
		a.metadata.TimeoutInSec,
		&maxBulkSubCount,
		a.metadata.MaxRetriableErrorsPerSec,
		a.metadata.MaxConcurrentHandlers,
		"topic "+req.Topic,
		a.metadata.LockRenewalInSec,
		requireSessions,
		a.logger,
	)

	receiveAndBlockFn := func(receiver impl.Receiver, onFirstSuccess func()) error {
		return sub.ReceiveBlocking(
			impl.GetBulkPubSubHandlerFunc(req.Topic, handler, a.logger, time.Duration(a.metadata.HandlerTimeoutInSec)*time.Second),
			receiver,
			onFirstSuccess,
			impl.ReceiveOptions{
				BulkEnabled:        true, // Bulk is supported in BulkSubscribe.
				SessionIdleTimeout: sessionIdleTimeout,
			},
		)
	}

	return a.doSubscribe(subscribeCtx, req, sub, receiveAndBlockFn, impl.SubscriptionOpts{
		RequireSessions:      requireSessions,
		MaxConcurrentSesions: maxConcurrentSessions,
	})
}

// doSubscribe is a helper function that handles the common logic for both Subscribe and BulkSubscribe.
// The receiveAndBlockFn is a function should invoke a blocking call to receive messages from the topic.
func (a *azureServiceBus) doSubscribe(subscribeCtx context.Context,
	req pubsub.SubscribeRequest, sub *impl.Subscription, receiveAndBlockFn func(impl.Receiver, func()) error, opts impl.SubscriptionOpts,
) error {
	// Does nothing if DisableEntityManagement is true
	err := a.client.EnsureSubscription(subscribeCtx, a.metadata.ConsumerID, req.Topic, opts)
	if err != nil {
		return err
	}

	// Reconnection backoff policy
	bo := backoff.NewExponentialBackOff()
	bo.MaxElapsedTime = 0
	bo.InitialInterval = time.Duration(a.metadata.MinConnectionRecoveryInSec) * time.Second
	bo.MaxInterval = time.Duration(a.metadata.MaxConnectionRecoveryInSec) * time.Second

	onFirstSuccess := func() {
		// Reset the backoff when the subscription is successful and we have received the first message
		bo.Reset()
	}

	go func() {
		// Reconnect loop.
		for {
			if opts.RequireSessions {
				a.ConnectAndReceiveWithSessions(subscribeCtx, req, sub, receiveAndBlockFn, onFirstSuccess, opts.MaxConcurrentSesions)
			} else {
				a.ConnectAndReceive(subscribeCtx, req, sub, receiveAndBlockFn, onFirstSuccess)
			}

			// If context was canceled, do not attempt to reconnect
			if subscribeCtx.Err() != nil {
				a.logger.Debug("Context canceled; will not reconnect")
				return
			}

			wait := bo.NextBackOff()
			a.logger.Warnf("Subscription to topic %s lost connection, attempting to reconnect in %s...", req.Topic, wait)
			time.Sleep(wait)
		}
	}()

	return nil
}

func (a *azureServiceBus) Close() (err error) {
	a.publishCancel()
	a.client.CloseAllSenders(a.logger)
	return nil
}

func (a *azureServiceBus) Features() []pubsub.Feature {
	return a.features
}

func (a *azureServiceBus) ConnectAndReceive(subscribeCtx context.Context, req pubsub.SubscribeRequest, sub *impl.Subscription, receiveAndBlockFn func(impl.Receiver, func()) error, onFirstSuccess func()) error {
	// The receiver context is used to tie the subscription context to
	// the lifetime of the receiver. This is necessary for shutting
	// down the lock renewal goroutine.
	receiverCtx, receiverCancel := context.WithCancel(subscribeCtx)
	defer receiverCancel()

	// Blocks until a successful connection (or until context is canceled)
	receiver, err := sub.Connect(func() (impl.Receiver, error) {
		a.logger.Debugf("Connecting to subscription %s for topic %s", a.metadata.ConsumerID, req.Topic)
		r, err := a.client.GetClient().NewReceiverForSubscription(req.Topic, a.metadata.ConsumerID, nil)
		return impl.NewMessageReceiver(r), err
	})
	if err != nil {
		// Realistically, the only time we should get to this point is if the context was canceled, but let's log any other error we may get.
		if !errors.Is(err, context.Canceled) {
			a.logger.Errorf("Could not instantiate session subscription %s to topic %s", a.metadata.ConsumerID, req.Topic)
		}
		return nil
	}
	defer func() {
		closeReceiverCtx, closeReceiverCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(a.metadata.TimeoutInSec))
		receiver.Close(closeReceiverCtx)
		closeReceiverCancel()
	}()

	// lock renewal loop
	go func() {
		a.logger.Debugf("Renewing locks for subscription %s for topic %s", a.metadata.ConsumerID, req.Topic)
		lockErr := sub.RenewLocksBlocking(receiverCtx, receiver, impl.LockRenewalOptions{
			RenewalInSec: a.metadata.LockRenewalInSec,
			TimeoutInSec: a.metadata.TimeoutInSec,
		})
		if lockErr != nil {
			a.logger.Error(lockErr)
		}
	}()

	a.logger.Debugf("Receiving messages from subscription %s for topic %s", a.metadata.ConsumerID, req.Topic)

	// receiveAndBlockFn will only return with an error that it cannot handle internally. The subscription connection is closed when this method returns.
	// If that occurs, we will log the error and attempt to re-establish the subscription connection until we exhaust the number of reconnect attempts.
	if err := receiveAndBlockFn(receiver, onFirstSuccess); err != nil {
		return err
	}

	// Gracefully close the connection (in case it's not closed already)
	// Use a background context here (with timeout) because ctx may be closed already.
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(a.metadata.TimeoutInSec))
	sub.Close(closeCtx)
	closeCancel()

	return nil
}

func (a *azureServiceBus) ConnectAndReceiveWithSessions(subscribeCtx context.Context, req pubsub.SubscribeRequest, sub *impl.Subscription, receiveAndBlockFn func(impl.Receiver, func()) error, onFirstSuccess func(), maxConcurrentSessions int) {
	sessionsChan := make(chan struct{}, maxConcurrentSessions)
	for i := 0; i < maxConcurrentSessions; i++ {
		sessionsChan <- struct{}{}
	}

	defer func() {
		// Gracefully close the connection (in case it's not closed already)
		// Use a background context here (with timeout) because ctx may be closed already
		closeSubCtx, closeSubCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(a.metadata.TimeoutInSec))
		sub.Close(closeSubCtx)
		closeSubCancel()
	}()

	for {
		select {
		case <-subscribeCtx.Done():
			return
		case <-sessionsChan:
			select {
			case <-subscribeCtx.Done():
				return
			default:
				go func() {
					// The receiver context controls the lifetime of the receiver.
					// Many receivers may existing within a single subscription
					// when sessions are required. We want to allow each receiver
					// to be independently cancellable.
					receiverCtx, receiverCancel := context.WithCancel(subscribeCtx)
					defer func() {
						receiverCancel()

						// Return the session to the pool
						a.logger.Debugf("Returning session to pool")
						sessionsChan <- struct{}{}
					}()

					var sessionID string

					// Blocks until a successful connection (or until context is canceled)
					receiver, err := sub.Connect(func() (impl.Receiver, error) {
						a.logger.Debugf("Accepting next available session subscription %s to topic %s", a.metadata.ConsumerID, req.Topic)
						r, err := a.client.GetClient().AcceptNextSessionForSubscription(receiverCtx, req.Topic, a.metadata.ConsumerID, nil)
						if err == nil && r != nil {
							sessionID = r.SessionID()
						}
						return impl.NewSessionReceiver(r), err
					})
					if err != nil {
						// Realistically, the only time we should get to this point is if the context was canceled, but let's log any other error we may get.
						if !errors.Is(err, context.Canceled) {
							a.logger.Errorf("Could not instantiate session subscription %s to topic %s", a.metadata.ConsumerID, req.Topic)
						}
						return
					}
					defer func() {
						a.logger.Debugf("Closing session %s receiver for subscription %s to topic %s", sessionID, a.metadata.ConsumerID, req.Topic)
						closeReceiverCtx, closeReceiverCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(a.metadata.TimeoutInSec))
						receiver.Close(closeReceiverCtx)
						closeReceiverCancel()
					}()

					// lock renewal loop
					go func() {
						a.logger.Debugf("Renewing locks for session %s receiver for subscription %s to topic %s", sessionID, a.metadata.ConsumerID, req.Topic)
						lockErr := sub.RenewLocksBlocking(receiverCtx, receiver, impl.LockRenewalOptions{
							RenewalInSec: a.metadata.LockRenewalInSec,
							TimeoutInSec: a.metadata.TimeoutInSec,
						})
						if lockErr != nil {
							a.logger.Error(lockErr)
						}
					}()

					a.logger.Debugf("Receiving messages for session %s receiver for subscription %s to topic %s", sessionID, a.metadata.ConsumerID, req.Topic)

					// receiveAndBlockFn will only return with an error that it cannot handle internally. The subscription connection is closed when this method returns.
					// If that occurs, we will log the error and attempt to re-establish the subscription connection until we exhaust the number of reconnect attempts.
					err = receiveAndBlockFn(receiver, onFirstSuccess)
					if err != nil && !errors.Is(err, context.Canceled) {
						a.logger.Error(err)
					}
				}()
			}
		}
	}
}
