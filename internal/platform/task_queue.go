package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const rabbitMQRetryRoutingKey = "image.retry"

type ImageTask struct {
	ImageID      string `json:"image_id"`
	OriginalPath string `json:"original_path"`
	DeviceID     string `json:"device_id"`
	EdgeNodeID   string `json:"edge_node_id"`
	OriginalName string `json:"original_name"`
	ThumbnailDir string `json:"thumbnail_dir"`
}

type RabbitMQTaskQueue struct {
	conn       *amqp.Connection
	channel    *amqp.Channel
	exchange   string
	queue      string
	routingKey string
	retryQueue string
}

func NewRabbitMQTaskQueue(cfg Config) (*RabbitMQTaskQueue, error) {
	conn, err := amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		return nil, err
	}
	channel, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	queue := &RabbitMQTaskQueue{
		conn:       conn,
		channel:    channel,
		exchange:   cfg.RabbitMQExchange,
		queue:      cfg.RabbitMQQueue,
		routingKey: cfg.RabbitMQRoutingKey,
		retryQueue: cfg.RabbitMQQueue + ".retry",
	}
	if err := queue.declare(cfg.RabbitMQRetryDelay); err != nil {
		_ = queue.Close()
		return nil, err
	}
	return queue, nil
}

func (q *RabbitMQTaskQueue) declare(retryDelay time.Duration) error {
	if err := q.channel.ExchangeDeclare(q.exchange, "direct", true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := q.channel.QueueDeclare(q.queue, true, false, false, false, amqp.Table{
		"x-dead-letter-exchange":    q.exchange,
		"x-dead-letter-routing-key": rabbitMQRetryRoutingKey,
	}); err != nil {
		return err
	}
	if err := q.channel.QueueBind(q.queue, q.routingKey, q.exchange, false, nil); err != nil {
		return err
	}
	if _, err := q.channel.QueueDeclare(q.retryQueue, true, false, false, false, amqp.Table{
		"x-message-ttl":             int32(retryDelay.Milliseconds()),
		"x-dead-letter-exchange":    q.exchange,
		"x-dead-letter-routing-key": q.routingKey,
	}); err != nil {
		return err
	}
	return q.channel.QueueBind(q.retryQueue, rabbitMQRetryRoutingKey, q.exchange, false, nil)
}

func (q *RabbitMQTaskQueue) PublishImageTask(ctx context.Context, task ImageTask) error {
	body, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return q.channel.PublishWithContext(ctx, q.exchange, q.routingKey, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Timestamp:    time.Now(),
		Body:         body,
	})
}

func (q *RabbitMQTaskQueue) ConsumeImageTasks(consumer string, prefetch int) (<-chan amqp.Delivery, error) {
	if prefetch < 1 {
		prefetch = 1
	}
	if err := q.channel.Qos(prefetch, 0, false); err != nil {
		return nil, err
	}
	return q.channel.Consume(q.queue, consumer, false, false, false, false, nil)
}

func (q *RabbitMQTaskQueue) NotifyClose(receiver chan *amqp.Error) chan *amqp.Error {
	return q.conn.NotifyClose(receiver)
}

func (q *RabbitMQTaskQueue) Close() error {
	var err error
	if q.channel != nil {
		err = q.channel.Close()
	}
	if q.conn != nil {
		if closeErr := q.conn.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}

func DecodeImageTask(body []byte) (ImageTask, error) {
	var task ImageTask
	if err := json.Unmarshal(body, &task); err != nil {
		return ImageTask{}, err
	}
	if task.ImageID == "" || task.OriginalPath == "" {
		return ImageTask{}, fmt.Errorf("invalid image task: image_id and original_path are required")
	}
	return task, nil
}

func RabbitMQRetryCount(headers amqp.Table, queueName string) int64 {
	deaths, ok := headers["x-death"].([]interface{})
	if !ok {
		return 0
	}
	var maxCount int64
	for _, raw := range deaths {
		death, ok := raw.(amqp.Table)
		if !ok {
			continue
		}
		if queue, _ := death["queue"].(string); queue != queueName {
			continue
		}
		switch count := death["count"].(type) {
		case int64:
			if count > maxCount {
				maxCount = count
			}
		case int32:
			if int64(count) > maxCount {
				maxCount = int64(count)
			}
		}
	}
	return maxCount
}
