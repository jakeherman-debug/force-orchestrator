const express = require('express');
const app = express();
const router = express.Router();

// --- basic CRUD via app ---
app.get('/users', getAllUsers);
app.post('/users', createUser);
app.get('/users/:id', getUser);
app.put('/users/:id', updateUser);
app.delete('/users/:id', deleteUser);

// --- router sub-resource ---
router.get('/profile', getProfile);
router.patch('/profile', updateProfile);

// --- health via app.all (should expand to 5 methods) ---
app.all('/health', healthCheck);

// --- dynamic template literal (should be skipped) ---
const prefix = '/v2';
app.get(`${prefix}/items`, listItems);

// Mount the router (not a route declaration — should not match)
app.use('/api', router);
