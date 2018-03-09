# Fields

* `store`
    * `type`, available values are: `simpledb`, `file`
    * `domainPostfix`, string.
    * `region`, refer to AWS region.

    For high availability, we recommend using AWS simpledb to store deployer's data.
    ```{json}
    {
        "store": {
        "type": "simpledb",
        "region": "us-east-1",
        "domainPostfix": "dev-swh"
        }
    }
    ```
* `shutDownTime`, string: You can specify when the deployer should shutdown automatically. Ex: "15"

* `incluster`, boolean.