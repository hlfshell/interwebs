import {defineConfig} from '@playwright/test';
export default defineConfig({
  testDir:'./tests', timeout:60000, workers:1,
  use:{headless:true},
  projects:[
    {name:'chromium',use:{browserName:'chromium',launchOptions:process.env.CHROMIUM_PATH?{executablePath:process.env.CHROMIUM_PATH}:{}}},
    {name:'firefox',use:{browserName:'firefox'}},
    {name:'webkit',use:{browserName:'webkit'}},
  ],
  webServer:{command:'npm run dev -- --host 127.0.0.1 --port 4173',url:'http://127.0.0.1:4173',reuseExistingServer:false},
});
