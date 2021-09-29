package ext

import "os"

var nodeIp string

func GetNodeIPFromEnv() string {
	if len(nodeIp) > 0 {
		return nodeIp
	} else {
		nodeIp = os.Getenv("VKUBELET_POD_IP")
	}
	return nodeIp
}
