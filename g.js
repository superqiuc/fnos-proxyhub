// proxyhub PAC (global)
var PROXY = 'PROXY 192.168.31.5:17928; DIRECT';
function FindProxyForURL(url, host) {
  if (isPlainHostName(host)) return "DIRECT";
  return PROXY;
}
