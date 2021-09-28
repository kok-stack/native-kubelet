package ext

import "os"

func GetNodeIPFromEnv() string {
	return os.Getenv("VKUBELET_POD_IP")
}
