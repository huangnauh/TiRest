package kafka

import (
	"bytes"
	"encoding/binary"
	"github.com/Shopify/sarama"
	"github.com/nsqio/go-diskqueue"
	"github.com/sirupsen/logrus"
	"gitlab.s.upyun.com/platform/tikv-proxy/config"
	"gitlab.s.upyun.com/platform/tikv-proxy/log"
	"gitlab.s.upyun.com/platform/tikv-proxy/store"
	"gitlab.s.upyun.com/platform/tikv-proxy/version"
	"os"
	"time"
)

const (
	MaxMessage = 1024
	MQ         = "kafka"
)

type Connector struct {
	producer  sarama.AsyncProducer
	log       *logrus.Entry
	queue     diskqueue.Interface
	writeBuf  bytes.Buffer
	writeChan chan store.KeyEntry
	conf      *config.Config
	//TODO: metrics
	//partitionOffset []struct {
	//	queued  uint64
	//	flushed uint64
	//	sent    uint64
	//}
}

type Driver struct {
}

func init() {
	store.RegisterConnector(Driver{})
}

func (d Driver) Name() string {
	return MQ
}

func (d Driver) Open(conf *config.Config) (store.Connector, error) {

	l := logrus.WithFields(logrus.Fields{
		"worker": "kafka connector",
	})

	if err := os.MkdirAll(conf.Connector.QueueDataPath, 0755); err != nil {
		l.Errorf("Failed to mkdir, %s", err)
		return nil, err
	}
	queue := diskqueue.New(version.APP, conf.Connector.QueueDataPath,
		conf.Connector.MaxBytesPerFile, 4, conf.Connector.MaxMsgSize,
		conf.Connector.SyncEvery, conf.Connector.SyncTimeout.Duration, log.NewLogFunc(l))

	conn := &Connector{
		queue:     queue,
		log:       l,
		writeChan: make(chan store.KeyEntry, MaxMessage),
		conf:      conf,
	}

	go conn.runQueue()

	if conf.Connector.EnableProducer {
		sarama.Logger = l
		c := sarama.NewConfig()

		backoff := func(retries, maxRetries int) time.Duration {
			b := conf.Connector.BackOff.Duration * time.Duration(retries+1)
			if b > conf.Connector.MaxBackOff.Duration {
				b = conf.Connector.MaxBackOff.Duration
			}
			return conf.Connector.MaxBackOff.Duration
		}
		c.Metadata.Full = conf.Connector.FetchMetadata
		c.Metadata.Retry.Max = conf.Connector.Retry
		c.Metadata.Retry.BackoffFunc = backoff

		c.Producer.RequiredAcks = sarama.WaitForLocal       // Only wait for the leader to ack
		c.Producer.Flush.Frequency = 500 * time.Millisecond // Flush batches every 500ms
		c.Producer.Retry.Max = conf.Connector.Retry
		c.Producer.Retry.BackoffFunc = backoff
		producer, err := sarama.NewAsyncProducer(conf.Connector.BrokerList, c)
		if err != nil {
			l.Errorf("Failed to start producer, %s", err)
			return nil, err
		}
		conn.producer = producer
		go conn.runProducer()
	}
	return conn, nil
}

func (c *Connector) putQueue(msg store.KeyEntry) error {
	c.writeBuf.Reset()
	keyLen := uint32(len(msg.Key))
	err := binary.Write(&c.writeBuf, binary.BigEndian, keyLen)
	if err != nil {
		c.log.Errorf("buffer write failed, %s", err)
		return err
	}
	_, err = c.writeBuf.Write(msg.Key)
	if err != nil {
		return err
	}
	_, err = c.writeBuf.Write(msg.Entry)
	if err != nil {
		return err
	}
	return c.queue.Put(c.writeBuf.Bytes())
}

func (c *Connector) runQueue() {
	timer := time.NewTimer(c.conf.Connector.WriteTimeout.Duration)
	for {
		select {
		case msg, ok := <-c.writeChan:
			if !ok {
				return
			}
			err := c.putQueue(msg)
			if err != nil {
				c.log.Errorf("put queue failed, %s", err)
				if !c.conf.Connector.EnableProducer {
					continue
				}

				input := &sarama.ProducerMessage{
					Topic: c.conf.Connector.Topic,
					Key:   sarama.ByteEncoder(msg.Key),
					Value: sarama.ByteEncoder(msg.Entry),
				}

				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(c.conf.Connector.WriteTimeout.Duration)
				select {
				case c.producer.Input() <- input:
				case <-timer.C:
					c.log.Errorf("put kafka timeout, %s", msg.Key)
				}
			}
		}
	}
}

func (c *Connector) runProducer() {
	for {
		select {
		case err, ok := <-c.producer.Errors():
			if !ok {
				return
			}
			c.log.Errorf("producer failed, %s", err)
		case body, ok := <-c.queue.ReadChan():
			if !ok {
				return
			}
			keyLen := binary.BigEndian.Uint32(body[:4])
			c.producer.Input() <- &sarama.ProducerMessage{
				Topic: c.conf.Connector.Topic,
				Key:   sarama.ByteEncoder(body[4 : keyLen+4]),
				Value: sarama.ByteEncoder(body[keyLen+4:]),
			}
		}
	}
}

func (c *Connector) Send(msg store.KeyEntry) error {
	c.writeChan <- msg
	return nil
}

func (c *Connector) Close() {
	close(c.writeChan)
	err := c.queue.Close()
	if err != nil {
		c.log.Errorf("queue close failed, %s", err)
	}
	if c.conf.Connector.EnableProducer {
		err = c.producer.Close()
		if err != nil {
			c.log.Errorf("producer close failed, %s", err)
		}
	}
}
