package metcap

import (
	"sync"
	"time"

	"gopkg.in/olivere/elastic.v3"
)

type Writer struct {
	Config    *WriterConfig
	Wg        *sync.WaitGroup
	Buffer    *Buffer
	Elastic   *elastic.Client
	Processor *elastic.BulkProcessor
	Logger    *Logger
	ExitChan  chan int
}



func NewWriter(c *WriterConfig, b *Buffer, wg *sync.WaitGroup, logger *Logger) *Writer {
	logger.Info("Initializing writer module")
	wg.Add(1)

	logger.Debugf("Connecting to ElasticSearch %v", c.Urls)
	es, err := elastic.NewClient(elastic.SetURL(c.Urls...))
	if err != nil {
		logger.Alertf("Can't connect to ElasticSearch: %v", err)
	}
	logger.Debug("Successfully connected to ElasticSearch")

	return &Writer{
		Config:    c,
		Wg:        wg,
		Buffer:    b,
		Elastic:   es,
		// Processor: processor,
		Logger:    logger,
		ExitChan:  make(chan int)}
}

func (w *Writer) Run() {
	w.Logger.Info("Starting writer module")
	defer w.Stop()

	var ES_TEMPLATE string = `{"template":"` + w.Config.Index + `*","mappings":{"raw":{"_source":{"enabled":false},"dynamic_templates":[{"fields":{"mapping":{"index":"not_analyzed","type":"string","copy_to":"@uniq"},"path_match":"fields.*"}}],"properties":{"@timestamp":{"type":"date","format":"strict_date_optional_time||epoch_millis"},"@uniq":{"type":"string","index":"not_analyzed"},"name":{"type":"string","index":"not_analyzed"},"value":{"type":"double","index":"not_analyzed"}}}}}`

	pipe := make(chan Metric, w.Config.BulkMax*w.Config.Concurrency*100)

	tmpl_exists, err := w.Elastic.IndexTemplateExists(w.Config.Index).Do()

	if err != nil {
		w.Logger.Alertf("Error checking index mapping template existence: %v", err)
	} else {
		if ! tmpl_exists {
			w.Logger.Infof("Index mapping template doesn't exits, creating '%s'", w.Config.Index)
			tmpl := w.Elastic.IndexPutTemplate(w.Config.Index).
				Create(true).
				BodyString(ES_TEMPLATE).
				Order(0)
			err := tmpl.Validate()
			if err != nil {
				w.Logger.Errorf("Failed to validate the index mapping template: %v", err)
			} else {
				res, err := tmpl.Do()
				if err != nil {
					w.Logger.Errorf("Failed to put the index mapping template: %v", err)
				} else {
					if ! res.Acknowledged {
						w.Logger.Error("Failed to acknowledge the new index mapping template")
					} else {
						w.Logger.Info("New index mapping template acknowledged")
					}
				}
			}
		}
	}

	w.Logger.Debug("Setting up bulk-processor")
	w.Processor, err = w.Elastic.BulkProcessor().
		BulkActions(w.Config.BulkMax).
		BulkSize(-1).
		Before(w.hookBeforeCommit).
		After(w.hookAfterCommit).
		FlushInterval(time.Duration(w.Config.BulkWait) * time.Second).
		Name("metrics-capacitor").
		Stats(true).
		Workers(w.Config.Concurrency).Do()

	if err != nil {
		w.Logger.Alertf("Failed to setup bulk-processor: %v", err)
	}

	for r := 0; r < w.Config.Concurrency; r++ {
		w.Logger.Debugf("Starting writer buffer-reader %2d", r+1)
		go w.readFromBuffer(pipe)
	}
	w.Logger.Info("Writer module started")

	for {
		metric := <-pipe
		w.Logger.Debug("Adding metric to bulk")
		req := elastic.NewBulkIndexRequest().
			Index(metric.Index(w.Config.Index)).
			Type(w.Config.DocType).
			Doc(string(metric.JSON()))
		w.Processor.Add(req)
	}
}

func (w *Writer) Stop() {
	w.Logger.Info("Stopping writer module")
	w.Processor.Flush()
	w.Processor.Close()
	w.Logger.Info("Writer module stopped")
	w.Wg.Done()
}

func (w *Writer) readFromBuffer(p chan Metric) {
	for {
		select {
		case <-w.ExitChan:
			break
		default:
			metric, err := w.Buffer.Pop()
			if err != nil {
				w.Logger.Error("Failed to BLPOP metric from buffer: " + err.Error())
			} else {
				p <- metric
				w.Logger.Debug("Popped metric from buffer")
			}
		}
	}
}

func (w *Writer) hookBeforeCommit(id int64, reqs []elastic.BulkableRequest) {
	w.Logger.Debugf("Writer committing %d requests", len(reqs))
}

func (w *Writer) hookAfterCommit(id int64, reqs []elastic.BulkableRequest, res *elastic.BulkResponse, err error) {
	w.Logger.Infof("Writer successfully commited %d metrics", len(res.Succeeded()))
	if len(res.Failed()) > 0 {
		w.Logger.Errorf("Writer failed to commit %d metrics", len(res.Failed()))
	}
	if err != nil {
		w.Logger.Error(err.Error())
	}
}
