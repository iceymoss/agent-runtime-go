package architecture

type Service struct{ store Store }

func (s Service) Get() string { return s.store.Load() }
