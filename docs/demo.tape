# Records docs/demo.gif. Run with: vhs docs/demo.tape
Output docs/demo.gif

Set Shell "bash"
Set FontSize 18
Set Width 1200
Set Height 720
Set Padding 24
Set Theme "Catppuccin Mocha"
Set TypingSpeed 45ms

Hide
Type "rm -rf /tmp/lazy-demo && mkdir -p /tmp/lazy-demo && cd /tmp/lazy-demo"
Enter
Type `echo '{"scripts":{"build":"node index.js"}}' > package.json`
Enter
Type `printf 'const total = add(2, 3);\nconsole.log(totl);\n\nfunction add(a, b) { return a + b; }\n' > index.js`
Enter
Type "clear"
Enter
Show

Type "lazy npm run build"
Sleep 400ms
Enter

Wait+Screen@90s /Apply this fix/
Sleep 3s
Type "a"
Sleep 400ms
Enter

Wait+Screen@60s /Fixed/
Sleep 4s
