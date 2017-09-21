package gcpgke

import "github.com/spf13/viper"

func getProjectId(serviceAccountPath string) (string, error) {
	viper := viper.New()
	viper.SetConfigType("json")
	viper.SetConfigFile(serviceAccountPath)
	err := viper.ReadInConfig()
	if err != nil {
		return "", err
	}
	return viper.GetString("project_id"), nil
}
