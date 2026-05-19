// TypeScript API calls fixture for D15 P5 consumer extractor tests.

import axios from 'axios';

// Fetch literal calls
async function loadUser(id: string) {
  const resp = await fetch('/api/users/123');
  return resp.json();
}

async function loadProfile() {
  const resp = await fetch("/api/profile");
  return resp.json();
}

// Fetch template literal — dynamic segment after /api/users/
async function loadUserById(id: string) {
  const resp = await fetch(`/api/users/${id}`);
  return resp.json();
}

// Axios method-style calls
async function createUser(data: object) {
  return axios.post('/api/users', data);
}

async function listUsers() {
  return axios.get('/api/users');
}

async function deleteUser(id: string) {
  return axios.delete(`/api/users/${id}`);
}

// Axios request-style call with url + method fields
async function updateUser(id: string, data: object) {
  return axios.request({
    url: '/api/v1/users',
    method: 'PUT',
    data,
  });
}

// Not an HTTP path — should not be extracted
const localRef = fetch('not-a-path');
const relativeRef = fetch('relative/path');
