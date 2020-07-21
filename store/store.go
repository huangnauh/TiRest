package store

import (
	"fmt"
	"github.com/sirupsen/logrus"
	"gitlab.s.upyun.com/platform/tikv-proxy/config"
	"gitlab.s.upyun.com/platform/tikv-proxy/utils"
	"gitlab.s.upyun.com/platform/tikv-proxy/utils/json"
	"gitlab.s.upyun.com/platform/tikv-proxy/xerror"
)

type DB interface {
	Close() error
	Put(key, val []byte) error
	UnsafeDelete(start, end []byte) error
	CheckAndPut(key, oldVal, newVal []byte, check CheckFunc) error
	Get(key []byte, option Option) (Value, error)
	BatchDelete(start, end []byte, limit int) ([]byte, int, error)
	List(start, end []byte, limit int, option Option) ([]KeyValue, error)
}

type CheckFunc func(oldVal, newVal, existVal []byte) ([]byte, error)

type KeyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Value struct {
	Secondary bool
	Value     []byte
}

var NoValue = Value{}

type KeyEntry struct {
	Key   []byte
	Entry []byte
}

type Option struct {
	ReplicaRead bool
	KeyOnly     bool
	Reverse     bool
	Secondary   []byte
}

var ReplicaReadOption = Option{ReplicaRead: true}
var KeyOnlyOption = Option{KeyOnly: true}
var NoOption = Option{}

type Connector interface {
	Close()
	Send(msg KeyEntry) error
}

type Store struct {
	db        DB
	connector Connector
	conf      *config.Config
	log       *logrus.Entry
}

type Log struct {
	Old string `json:"old"`
	New string `json:"new"`
}

type DBDriver interface {
	Name() string
	Open(conf *config.Config) (DB, error)
}

type ConnectorDriver interface {
	Name() string
	Open(conf *config.Config) (Connector, error)
}

var dDrivers = make(map[string]DBDriver)
var cDrivers = make(map[string]ConnectorDriver)

func RegisterDB(driver DBDriver) {
	name := driver.Name()
	if _, ok := dDrivers[name]; ok {
		panic(fmt.Errorf("store %s is already registered", name))
	}

	dDrivers[name] = driver
}

func RegisterConnector(driver ConnectorDriver) {
	name := driver.Name()
	if _, ok := cDrivers[name]; ok {
		panic(fmt.Errorf("store %s is already registered", name))
	}

	cDrivers[name] = driver
}

func NewStore(conf *config.Config) (*Store, error) {
	_, ok := cDrivers[conf.Connector.Name]
	if !ok {
		return nil, xerror.ErrNotRegister
	}
	_, ok = dDrivers[conf.Store.Name]
	if !ok {
		return nil, xerror.ErrNotRegister
	}
	return &Store{
		conf: conf,
		log:  logrus.WithFields(logrus.Fields{"worker": "store"}),
	}, nil
}

func (s *Store) Open() {
	go func() {
		cDriver := cDrivers[s.conf.Connector.Name]
		connector, err := cDriver.Open(s.conf)
		if err != nil {
			s.log.Errorf("open connector %s failed, %s", s.conf.Connector.Name, err)
		} else {
			s.connector = connector
		}
	}()

	go func() {
		dDriver := dDrivers[s.conf.Store.Name]
		db, err := dDriver.Open(s.conf)
		if err != nil {
			s.log.Errorf("open db %s failed, %s", s.conf.Store.Name, err)
		} else {
			s.db = db
		}
	}()
}

func (s *Store) Close() error {
	if s.connector != nil {
		logrus.Infof("close connector %s", s.conf.Connector.Name)
		s.connector.Close()
	}
	if s.db != nil {
		logrus.Infof("close db %s", s.conf.Store.Name)
		return s.db.Close()
	}
	return nil
}

func (s *Store) Health() error {
	if s.db == nil {
		return xerror.ErrDatabaseNotExists
	}
	return nil
}

func (s *Store) Get(key []byte, opt Option) (Value, error) {
	if s.db == nil {
		return NoValue, xerror.ErrNotExists
	}
	v, err := s.db.Get(key, opt)
	if err != nil && err != xerror.ErrNotExists {
		s.log.Errorf("get key %s failed, %s", key, err)
		return NoValue, err
	}
	s.log.Debugf("key %s value %s", key, v)
	return v, nil
}

func (s *Store) CheckAndPut(key, entry []byte, check CheckFunc) error {
	if s.db == nil {
		return xerror.ErrNotExists
	}

	if len(entry) == 0 {
		s.log.Errorf("key %s cas need body", key)
		return xerror.ErrNotExists
	}

	l := &Log{}
	err := json.Unmarshal(entry, &l)
	if err != nil {
		s.log.Errorf("key %s cas invalid, %s", key, err)
		return err
	}

	s.log.Debugf("key %s old %s new %s", key, l.Old, l.New)
	err = s.db.CheckAndPut(key, utils.S2B(l.Old), utils.S2B(l.New), check)
	if err != nil {
		s.log.Errorf("key %s cas failed, %s", key, err)
		return err
	}

	if entry != nil && s.connector != nil {
		s.connector.Send(KeyEntry{Key: key, Entry: entry})
	}
	return nil
}

func (s *Store) List(start, end []byte, limit int, option Option) ([]KeyValue, error) {
	if s.db == nil {
		return nil, xerror.ErrNotExists
	}

	res, err := s.db.List(start, end, limit, option)
	if err != nil {
		s.log.Errorf("list (%s-%s) limit %d, %s", start, end, limit, err)
		return nil, err
	}
	return res, nil
}

func (s *Store) BatchDelete(start, end []byte, limit int) ([]byte, int, error) {
	if s.db == nil {
		return nil, 0, xerror.ErrNotExists
	}

	lastKey, deleted, err := s.db.BatchDelete(start, end, limit)
	if err != nil {
		s.log.Errorf("deleted %d (%s-%s) limit %d err %s", deleted, start, end, limit, err)
		return lastKey, deleted, err
	}
	s.log.Infof("deleted %d (%s-%s)", deleted, start, end)
	return lastKey, deleted, nil
}

func (s *Store) UnsafeDelete(start, end []byte) {
	s.log.Infof("unsafe deleted (%s-%s)", start, end)
	err := s.db.UnsafeDelete(start, end)
	if err != nil {
		s.log.Errorf("unsafe deleted (%s-%s), err %s", start, end, err)
	}
}
