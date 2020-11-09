package agent

//networking cni-plugin
type CNI interface {
	Prepare()
	Run()
}
