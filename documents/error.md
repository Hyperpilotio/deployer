# Error Handling

* Case 1.
    For our own functiion which does not include any third party package or reply on any dependency,
    return error message directly.
* Case 2.
    For a function which relies on third party package, return a wrapped message. Like, fmt.Errorf("Unable to ... %s", err.Error())


# Details of error handling in file awsecs/deploy.go

* An important error or error with high severity, return the error directly.
* Error which does not affect other functions, throw / display a warning message. Like: the failure of deleteTaskDefinitions function
    would not affect deleteCluster function, it displays a warning message. (glog.Warningf("....")). On the other hand,
    the failure which affects other functions returns a error message.
