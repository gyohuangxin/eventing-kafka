/*
Copyright 2018 The Knative Authors

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
package dispatcher

import (
	"context"
	"errors"
	"fmt"
	nethttp "net/http"
	"strings"
	"sync"
	"sync/atomic"

	"knative.dev/eventing-kafka/pkg/source"

	"knative.dev/eventing-kafka/pkg/common/tracing"

	"github.com/Shopify/sarama"
	protocolkafka "github.com/cloudevents/sdk-go/protocol/kafka_sarama/v2"
	"github.com/cloudevents/sdk-go/v2/binding"
	"github.com/google/uuid"
	"go.opencensus.io/trace"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/eventing-kafka/pkg/channel/consolidated/utils"
	"knative.dev/eventing-kafka/pkg/channel/distributed/common/env"
	"knative.dev/eventing-kafka/pkg/common/consumer"
	eventingchannels "knative.dev/eventing/pkg/channel"
	"knative.dev/eventing/pkg/channel/fanout"
	"knative.dev/eventing/pkg/kncloudevents"
	"knative.dev/pkg/kmeta"
)

type KafkaDispatcher struct {
	hostToChannelMap atomic.Value
	// hostToChannelMapLock is used to update hostToChannelMap
	hostToChannelMapLock sync.Mutex

	receiver   *eventingchannels.MessageReceiver
	dispatcher *eventingchannels.MessageDispatcherImpl

	kafkaSyncProducer    sarama.SyncProducer
	channelSubscriptions map[eventingchannels.ChannelReference][]types.UID
	subsConsumerGroups   map[types.UID]sarama.ConsumerGroup
	subscriptions        map[types.UID]Subscription
	// consumerUpdateLock must be used to update kafkaConsumers
	consumerUpdateLock   sync.Mutex
	kafkaConsumerFactory consumer.KafkaConsumerGroupFactory

	topicFunc TopicFunc
	logger    *zap.SugaredLogger
}

type Subscription struct {
	UID types.UID
	fanout.Subscription
}

func (sub Subscription) String() string {
	var s strings.Builder
	s.WriteString("UID: " + string(sub.UID))
	s.WriteRune('\n')
	if sub.Subscriber != nil {
		s.WriteString("Subscriber: " + sub.Subscriber.String())
		s.WriteRune('\n')
	}
	if sub.Reply != nil {
		s.WriteString("Reply: " + sub.Reply.String())
		s.WriteRune('\n')
	}
	if sub.DeadLetter != nil {
		s.WriteString("DeadLetter: " + sub.DeadLetter.String())
		s.WriteRune('\n')
	}
	return s.String()
}

func NewDispatcher(ctx context.Context, args *KafkaDispatcherArgs) (*KafkaDispatcher, error) {
	conf := sarama.NewConfig()
	conf.Version = sarama.V2_0_0_0
	conf.ClientID = args.ClientID
	conf.Consumer.Return.Errors = true    // Returns the errors in ConsumerGroup#Errors() https://godoc.org/github.com/Shopify/sarama#ConsumerGroup
	conf.Producer.Return.Successes = true // Must be enabled for sync producer

	// Get the auth info
	if args.KafkaAuthConfig != nil {
		// tls
		if args.KafkaAuthConfig.TLS != nil {
			conf.Net.TLS.Enable = true
			tlsConfig, err := source.NewTLSConfig(args.KafkaAuthConfig.TLS.Usercert, args.KafkaAuthConfig.TLS.Userkey, args.KafkaAuthConfig.TLS.Cacert)
			if err != nil {
				return nil, err
			}
			conf.Net.TLS.Config = tlsConfig
		}
		// SASL
		if args.KafkaAuthConfig.SASL != nil {
			conf.Net.SASL.Enable = true
			conf.Net.SASL.Handshake = true

			// if SASLTypeSCRAMSHA256 is provided we use that. Defaulting to SASLTypeSCRAMSHA512
			if args.KafkaAuthConfig.SASL.SaslType == utils.SASLTypeSCRAMSHA256 {
				conf.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &source.XDGSCRAMClient{HashGeneratorFcn: source.SHA256} }
				conf.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA256

			} else {
				conf.Net.SASL.SCRAMClientGeneratorFunc = func() sarama.SCRAMClient { return &source.XDGSCRAMClient{HashGeneratorFcn: source.SHA512} }
				conf.Net.SASL.Mechanism = sarama.SASLTypeSCRAMSHA512
			}

			conf.Net.SASL.User = args.KafkaAuthConfig.SASL.User
			conf.Net.SASL.Password = args.KafkaAuthConfig.SASL.Password
		}
	}

	producer, err := sarama.NewSyncProducer(args.Brokers, conf)
	if err != nil {
		return nil, fmt.Errorf("unable to create kafka producer against Kafka bootstrap servers %v : %v", args.Brokers, err)
	}

	dispatcher := &KafkaDispatcher{
		dispatcher:           eventingchannels.NewMessageDispatcher(args.Logger.Desugar()),
		kafkaConsumerFactory: consumer.NewConsumerGroupFactory(args.Brokers, conf),
		channelSubscriptions: make(map[eventingchannels.ChannelReference][]types.UID),
		subsConsumerGroups:   make(map[types.UID]sarama.ConsumerGroup),
		subscriptions:        make(map[types.UID]Subscription),
		kafkaSyncProducer:    producer,
		logger:               args.Logger,
		topicFunc:            args.TopicFunc,
	}

	podName, err := env.GetRequiredConfigValue(args.Logger.Desugar(), env.PodNameEnvVarKey)
	if err != nil {
		return nil, err
	}
	containerName, err := env.GetRequiredConfigValue(args.Logger.Desugar(), env.ContainerNameEnvVarKey)
	if err != nil {
		return nil, err
	}
	reporter := eventingchannels.NewStatsReporter(containerName, kmeta.ChildName(podName, uuid.New().String()))
	receiverFunc, err := eventingchannels.NewMessageReceiver(
		func(ctx context.Context, channel eventingchannels.ChannelReference, message binding.Message, transformers []binding.Transformer, _ nethttp.Header) error {
			kafkaProducerMessage := sarama.ProducerMessage{
				Topic: dispatcher.topicFunc(utils.KafkaChannelSeparator, channel.Namespace, channel.Name),
			}

			dispatcher.logger.Debugw("Received a new message from MessageReceiver, dispatching to Kafka", zap.Any("channel", channel))
			err := protocolkafka.WriteProducerMessage(ctx, message, &kafkaProducerMessage, transformers...)
			if err != nil {
				return err
			}

			kafkaProducerMessage.Headers = append(kafkaProducerMessage.Headers, tracing.SerializeTrace(trace.FromContext(ctx).SpanContext())...)

			partition, offset, err := dispatcher.kafkaSyncProducer.SendMessage(&kafkaProducerMessage)

			if err == nil {
				dispatcher.logger.Debugw("message sent", zap.Int32("partition", partition), zap.Int64("offset", offset))
			} else {
				dispatcher.logger.Warnw("message not sent", zap.Error(err))
			}

			return err
		},
		args.Logger.Desugar(),
		reporter,
		eventingchannels.ResolveMessageChannelFromHostHeader(dispatcher.getChannelReferenceFromHost))
	if err != nil {
		return nil, err
	}

	dispatcher.receiver = receiverFunc
	dispatcher.setHostToChannelMap(map[string]eventingchannels.ChannelReference{})
	return dispatcher, nil
}

type TopicFunc func(separator, namespace, name string) string

type KafkaDispatcherArgs struct {
	KnCEConnectionArgs *kncloudevents.ConnectionArgs
	ClientID           string
	Brokers            []string
	KafkaAuthConfig    *utils.KafkaAuthConfig
	TopicFunc          TopicFunc
	Logger             *zap.SugaredLogger
}

type consumerMessageHandler struct {
	logger     *zap.SugaredLogger
	sub        Subscription
	dispatcher *eventingchannels.MessageDispatcherImpl
}

func (c consumerMessageHandler) Handle(ctx context.Context, consumerMessage *sarama.ConsumerMessage) (bool, error) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Warn("Panic happened while handling a message",
				zap.String("topic", consumerMessage.Topic),
				zap.Any("panic value", r),
			)
		}
	}()
	message := protocolkafka.NewMessageFromConsumerMessage(consumerMessage)
	if message.ReadEncoding() == binding.EncodingUnknown {
		return false, errors.New("received a message with unknown encoding")
	}

	c.logger.Debug("Going to dispatch the message",
		zap.String("topic", consumerMessage.Topic),
		zap.String("subscription", c.sub.String()),
	)

	ctx, span := tracing.StartTraceFromMessage(c.logger, ctx, message, consumerMessage.Topic)
	defer span.End()

	_, err := c.dispatcher.DispatchMessageWithRetries(
		ctx,
		message,
		nil,
		c.sub.Subscriber,
		c.sub.Reply,
		c.sub.DeadLetter,
		c.sub.RetryConfig,
	)

	// NOTE: only return `true` here if DispatchMessage actually delivered the message.
	return err == nil, err
}

var _ consumer.KafkaConsumerHandler = (*consumerMessageHandler)(nil)

type Config struct {
	// The configuration of each channel in this handler.
	ChannelConfigs []ChannelConfig
}

type ChannelConfig struct {
	Namespace     string
	Name          string
	HostName      string
	Subscriptions []Subscription
}

// UpdateKafkaConsumers will be called by new CRD based kafka channel dispatcher controller.
func (d *KafkaDispatcher) UpdateKafkaConsumers(config *Config) (map[types.UID]error, error) {
	if config == nil {
		return nil, fmt.Errorf("nil config")
	}

	d.consumerUpdateLock.Lock()
	defer d.consumerUpdateLock.Unlock()

	var newSubs []types.UID
	failedToSubscribe := make(map[types.UID]error)
	for _, cc := range config.ChannelConfigs {
		channelRef := eventingchannels.ChannelReference{
			Name:      cc.Name,
			Namespace: cc.Namespace,
		}
		for _, subSpec := range cc.Subscriptions {
			newSubs = append(newSubs, subSpec.UID)

			// Check if sub already exists
			exists := false
			for _, s := range d.channelSubscriptions[channelRef] {
				if s == subSpec.UID {
					exists = true
				}
			}

			if !exists {
				// only subscribe when not exists in channel-subscriptions map
				// do not need to resubscribe every time channel fanout config is updated
				if err := d.subscribe(channelRef, subSpec); err != nil {
					failedToSubscribe[subSpec.UID] = err
				}
			}
		}
	}

	d.logger.Debug("Number of new subs", zap.Any("subs", len(newSubs)))
	d.logger.Debug("Number of subs failed to subscribe", zap.Any("subs", len(failedToSubscribe)))

	// Unsubscribe and close consumer for any deleted subscriptions
	for channelRef, subs := range d.channelSubscriptions {
		for _, oldSub := range subs {
			removedSub := true
			for _, s := range newSubs {
				if s == oldSub {
					removedSub = false
				}
			}

			if removedSub {
				if err := d.unsubscribe(channelRef, d.subscriptions[oldSub]); err != nil {
					return nil, err
				}
			}
		}
		d.channelSubscriptions[channelRef] = newSubs
	}
	return failedToSubscribe, nil
}

// UpdateHostToChannelMap will be called by new CRD based kafka channel dispatcher controller.
func (d *KafkaDispatcher) UpdateHostToChannelMap(config *Config) error {
	if config == nil {
		return errors.New("nil config")
	}

	d.hostToChannelMapLock.Lock()
	defer d.hostToChannelMapLock.Unlock()

	hcMap, err := createHostToChannelMap(config)
	if err != nil {
		return err
	}

	d.setHostToChannelMap(hcMap)
	return nil
}

func createHostToChannelMap(config *Config) (map[string]eventingchannels.ChannelReference, error) {
	hcMap := make(map[string]eventingchannels.ChannelReference, len(config.ChannelConfigs))
	for _, cConfig := range config.ChannelConfigs {
		if cr, ok := hcMap[cConfig.HostName]; ok {
			return nil, fmt.Errorf(
				"duplicate hostName found. Each channel must have a unique host header. HostName:%s, channel:%s.%s, channel:%s.%s",
				cConfig.HostName,
				cConfig.Namespace,
				cConfig.Name,
				cr.Namespace,
				cr.Name)
		}
		hcMap[cConfig.HostName] = eventingchannels.ChannelReference{Name: cConfig.Name, Namespace: cConfig.Namespace}
	}
	return hcMap, nil
}

// Start starts the kafka dispatcher's message processing.
func (d *KafkaDispatcher) Start(ctx context.Context) error {
	if d.receiver == nil {
		return fmt.Errorf("message receiver is not set")
	}

	return d.receiver.Start(ctx)
}

// subscribe reads kafkaConsumers which gets updated in UpdateConfig in a separate go-routine.
// subscribe must be called under updateLock.
func (d *KafkaDispatcher) subscribe(channelRef eventingchannels.ChannelReference, sub Subscription) error {
	d.logger.Info("Subscribing", zap.Any("channelRef", channelRef), zap.Any("subscription", sub.UID))

	topicName := d.topicFunc(utils.KafkaChannelSeparator, channelRef.Namespace, channelRef.Name)
	groupID := fmt.Sprintf("kafka.%s.%s.%s", channelRef.Namespace, channelRef.Name, string(sub.UID))

	handler := &consumerMessageHandler{d.logger, sub, d.dispatcher}

	consumerGroup, err := d.kafkaConsumerFactory.StartConsumerGroup(groupID, []string{topicName}, d.logger, handler)

	if err != nil {
		// we can not create a consumer - logging that, with reason
		d.logger.Infow("Could not create proper consumer", zap.Error(err))
		return err
	}

	// sarama reports error in consumerGroup.Error() channel
	// this goroutine logs errors incoming
	go func() {
		for err = range consumerGroup.Errors() {
			d.logger.Warnw("Error in consumer group", zap.Error(err))
		}
	}()

	d.channelSubscriptions[channelRef] = append(d.channelSubscriptions[channelRef], sub.UID)
	d.subscriptions[sub.UID] = sub
	d.subsConsumerGroups[sub.UID] = consumerGroup

	return nil
}

// unsubscribe reads kafkaConsumers which gets updated in UpdateConfig in a separate go-routine.
// unsubscribe must be called under updateLock.
func (d *KafkaDispatcher) unsubscribe(channel eventingchannels.ChannelReference, sub Subscription) error {
	d.logger.Infow("Unsubscribing from channel", zap.Any("channel", channel), zap.String("subscription", sub.String()))
	delete(d.subscriptions, sub.UID)
	if subsSlice, ok := d.channelSubscriptions[channel]; ok {
		var newSlice []types.UID
		for _, oldSub := range subsSlice {
			if oldSub != sub.UID {
				newSlice = append(newSlice, oldSub)
			}
		}
		d.channelSubscriptions[channel] = newSlice
	}
	if consumer, ok := d.subsConsumerGroups[sub.UID]; ok {
		delete(d.subsConsumerGroups, sub.UID)
		return consumer.Close()
	}
	return nil
}

func (d *KafkaDispatcher) getHostToChannelMap() map[string]eventingchannels.ChannelReference {
	return d.hostToChannelMap.Load().(map[string]eventingchannels.ChannelReference)
}

func (d *KafkaDispatcher) setHostToChannelMap(hcMap map[string]eventingchannels.ChannelReference) {
	d.hostToChannelMap.Store(hcMap)
}

func (d *KafkaDispatcher) getChannelReferenceFromHost(host string) (eventingchannels.ChannelReference, error) {
	chMap := d.getHostToChannelMap()
	cr, ok := chMap[host]
	if !ok {
		return cr, eventingchannels.UnknownHostError(host)
	}
	return cr, nil
}
