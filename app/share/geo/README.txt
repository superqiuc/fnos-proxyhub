代理中心内置地理数据

来源：MetaCubeX/meta-rules-dat（release 分支）
      https://github.com/MetaCubeX/meta-rules-dat
下载时间：2026-11-22

文件：
- Country.mmdb：GEOIP 规则使用的国家 IP 库（默认 geodata-mode）
- GeoSite.dat：GEOSITE 域名规则
- GeoIP.dat：geodata-mode: true 时使用的国家规则

首次启动时，程序会把这里的文件复制到应用数据目录；如果数据目录里已有
有效文件则跳过。文件缺失或损坏时，会自动从 jsDelivr / gcore / ghproxy /
GitHub 重新下载。

许可证与更新说明见上游仓库。
