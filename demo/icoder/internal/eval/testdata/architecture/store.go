package architecture

type Store struct{}

func (Store) Load() string { return "data" }
