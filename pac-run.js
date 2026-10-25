const fs=require('fs');
const code=fs.readFileSync(__dirname+'/pac-test.js','utf8');
const fn=new Function('isPlainHostName','dnsDomainIs', code+'; return FindProxyForURL;');
const f=fn(h=>h.indexOf('.')<0,(h,d)=>h===d||h.endsWith(d));
for (const h of ['www.google.com','www.youtube.com','baidu.com','www.qq.com','pypi.org','example.cn']) console.log(h,'->',f('https://'+h+'/',h));
