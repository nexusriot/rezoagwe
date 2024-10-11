## R3zo Agwe

_Just a Pre-PoC concept of very simple distributed KV storage._

### **Why?**

![Pic](https://github.com/nexusriot/rezoagwe/blob/main/wtf.png)

### **Design (concept)**

![Pic](https://github.com/nexusriot/rezoagwe/blob/main/rezo_agwe.png)


![Pic](https://github.com/nexusriot/rezoagwe/blob/main/bootstrap.png)


![Pic](https://github.com/nexusriot/rezoagwe/blob/main/discovery.png)

### **Usage:**

```
./bootstrap [-port number]
```
```
./bootstrap -port 9999
```
by default port is __9999__

```
./discovery [-bootstrap addres] [-node address]
```

```
./discovery -bootstrap :9999 -node :3138
```
by default bootstrap is __:9999__, node address is __:3137__

