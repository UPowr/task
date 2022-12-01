package read

import (
	"os"
	"path/filepath"
)

// searchForFile checks the working directory and every parent directory to
// find a configuration file with the default name.
// This should be called when an explicit file is not passed in to determine
// the full path to the relevant config file.
func searchForFile(dirPath string, fileName string) (fullPath string, found bool, err error) {

	if dirPath == "" {
		dirPath, err = os.Getwd()
		if err != nil {
			return "", false, err
		}
	}

	lastFound := ""
	prevPath := ""
	for dirPath != prevPath {
		fullPath, found, err = findFileInDir(fileName, dirPath)
		if found {
			lastFound = fullPath
		}
		if err != nil {
			return "", false, err
		}
		prevPath, dirPath = dirPath, filepath.Dir(dirPath)
	}

	return lastFound, lastFound != "", nil
}

func findFileInDir(fileName string, dirPath string) (fullPath string, found bool, err error) {

	fullPath, found, err = findFileInDirByName(dirPath, fileName)
	if err != nil || found {
		return fullPath, found, err
	}

	return "", false, nil
}

func findFileInDirByName(dirPath, fileName string) (fullPath string, found bool, err error) {

	fullPath = filepath.Join(dirPath, fileName)
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}

	return fullPath, true, nil
}
